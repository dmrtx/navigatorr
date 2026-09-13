# Plan de implementación: optimización inteligente de transcodificación

Estado: en progreso por fases (un solo PR hacia main, sin merge automático)  
Proyecto: Navigatorr  
Rama objetivo: `main`  
Fecha del plan: 2026-09-13  
Última actualización: 2026-09-13 (fases 0-3 y núcleo puro de fases 4-5 integrados)

## 1. Objetivo

Extender Navigatorr con un modo de optimización que inspeccione el archivo original, pruebe un conjunto pequeño de configuraciones sobre samples deterministas, mida calidad objetiva, estime el tamaño completo y seleccione parámetros de encoding de manera explicable.

La calidad tiene prioridad sobre alcanzar un tamaño fijo. Los rangos de tamaño son restricciones suaves, no targets obligatorios.

## 2. Reglas invariables de seguridad

- Trabajar únicamente en Navigatorr.
- No modificar Ansible ni infraestructura externa desde este repositorio.
- No realizar cambios manuales en servidores fuera de los mecanismos existentes.
- No borrar, reemplazar ni sobrescribir archivos originales.
- Mantener `replace_original=false` y el rechazo fail-closed de `replace_original=true`.
- Mantener compatibilidad con recipes v1, perfiles existentes y `transcode_media` v2.
- No aceptar argumentos FFmpeg arbitrarios en recipes.
- Detectar capacidades reales del FFmpeg y del worker antes de utilizar una opción.
- No inventar scores ni convertir thresholds de una métrica en thresholds de otra.
- Cubrir cada cambio con tests proporcionales al riesgo.
- Crear un único PR hacia `main` al terminar la entrega completa por fases; no hacer merge automático.

## 3. Arquitectura actual confirmada

- `mediainspect.InspectDetailed` realiza el preflight local mediante `ffprobe`.
- `transcode/recipe` carga recipes estrictas, resuelve perfiles y genera un `transcode.Plan` inmutable con digest.
- `transcode.Executor` coordina el worker mediante SSH con operaciones `Doctor`, `Submit`, `Status` y `Cancel`.
- `internal/transcodeworker` valida el plan, vuelve a inspeccionar streams, construye argv seguro y ejecuta FFmpeg.
- El worker ya detecta capacidades de `hevc_videotoolbox` para profiles, pixel formats, `prio_speed`, `spatial_aq` y `realtime`.
- `transcode_media` v2 es una action persistente con pasos de preflight, submit, wait, validate y accept.
- Las actions persisten estado, outputs y logs de pasos en SQLite.
- `transcode_batch` persiste cada item y reutiliza `transcode_media` como child action.
- `profile=auto` utiliza metadata contextual (`is_anime`/`media_type`) y reglas deterministas; no clasifica visualmente el contenido.
- El worker solo admite actualmente `hevc_videotoolbox` y audio `copy`.

## 4. Decisiones de diseño

### 4.1 Estrategia de entrega: un solo PR hacia main

La implementación se consolida en **un único PR hacia `main`** sin merge automático, manteniendo una estricta granularidad de commits por fase:

1. **Fundación (fases 0-3)**: inspección ampliada, handshake de capacidades versionado y schema recipe v2 retrocompatible.
2. **Núcleo puro de optimización (fases 4-5 puro)**: paquete Go puro sin I/O externo (`transcode/optimization`) para sampling determinista, evaluación de calidad VMAF/SSIM, estimación de tamaño y selección determinista de candidatos.
3. **Integración VideoToolbox (fases 4-8)**: workspace de sampling en worker, benchmarking FFmpeg, action `benchmark_transcode`, integración con `transcode_media`, documentación y validación candidate-only.
4. **Eficiencia avanzada (fases posteriores en el mismo PR)**: encoder `libx265`, perfiles x265, política avanzada de audio por stream, caché de benchmarks y optimizaciones de reutilización batch como commits sucesivos dentro del mismo PR.

Se elimina la división obligatoria en dos PRs separados para simplificar la revisión y garantizar la trazabilidad completa en un solo branch de entrega.

### 4.2 Elementos fuera del hito inicial VideoToolbox

- x265 (se implementará como commits posteriores en este mismo PR);
- transcodificación avanzada de audio (commits posteriores);
- conversión automática 8-bit a 10-bit;
- optimización automática de HDR o Dolby Vision;
- detección avanzada de escenas, intros, créditos o pantallas negras;
- caché de benchmarks (commits posteriores);
- reutilización de un benchmark entre episodios (commits posteriores).

### 4.3 Nombres de perfiles

No cambiar el significado de perfiles existentes.

En particular, `anime-hevc-quality` seguirá siendo VideoToolbox. Los futuros perfiles software utilizarán nombres inequívocos:

- `anime-x265-quality`;
- `general-x265-quality`.

### 4.4 Evolución del schema

- Introducir `schema_version: 2` para recipes con optimización.
- El nuevo loader debe aceptar recipes v1 y v2 (`MinSchemaVersion` = 1, `LatestSchemaVersion` = 2).
- Las recipes v1 deben normalizarse al modelo interno sin cambiar su comportamiento.
- Una recipe v2 no debe activarse en un binario antiguo.
- El rollout debe actualizar primero Navigatorr y el worker, verificar protocolo/capacidades y después activar la recipe v2.
- Los overrides locales bajo `transcode.profiles` deben conservar compatibilidad y mapear correctamente los campos soportados.

### 4.5 Responsabilidades

- Navigatorr coordina, persiste y explica la decisión.
- El worker ejecuta inspecciones dependientes de su FFmpeg, genera samples y calcula métricas.
- La política de búsqueda vive en la recipe.
- El resultado del benchmark contiene los parámetros exactos necesarios para reproducir el ganador.
- El `PlanDigest` del transcode final representa el plan ganador concreto, no solo la política de búsqueda.

## 5. Diseño funcional de optimización VideoToolbox

### 5.1 Pipeline

```text
preflight + InspectDetailed
→ capability handshake
→ resolver política de optimización
→ SamplePlanner
→ benchmark remoto secuencial
→ QualityEvaluator
→ OutputEstimator
→ CandidateSelector
→ persistir informe y explicación
→ benchmark-only: finalizar
→ optimization enabled: crear plan ganador inmutable
→ transcode normal
→ validación existente
→ aceptación candidate-only
```

Los perfiles sin `optimization.enabled` recorren el flujo actual sin cambios observables.

### 5.2 Source inspection

Extender `mediainspect.DetailedReport` y su modelo de streams para conservar, como mínimo:

- codec y profile;
- resolución;
- pixel format y bit depth;
- frame rate racional y calculado;
- duración, tamaño y bitrate general;
- color range, color space, color primaries y color transfer;
- metadata HDR disponible, mastering display, content light y side data relevante;
- codec, idioma, channels, channel layout, bitrate y flags de cada audio;
- codec, idioma y flags de cada subtítulo;
- attachments y chapters.

La información compartida debe provenir de un modelo común; no crear inspectores incompatibles entre Navigatorr y el worker.

### 5.3 Reglas de bit depth

- Source 10-bit: solo considerar candidatos 10-bit compatibles.
- Source 8-bit: solo considerar candidatos 8-bit.
- No describir Main10 existente como una optimización.
- Si el worker no puede preservar el bit depth requerido, devolver `review` o fallo explícito; nunca degradar silenciosamente.
- Mantener la validación posterior del bit depth del candidato final.

### 5.4 HDR

Soporte de selección automática únicamente para SDR.

Para HDR o Dolby Vision:

- conservar y reportar la metadata detectada;
- no aplicar thresholds SDR silenciosamente;
- devolver `review`, o un benchmark explícitamente marcado como informativo y sin ganador automático;
- no hacer tone mapping implícito.

### 5.5 Capability handshake

Generalizar la respuesta de capacidades para incluir:

- versión del protocolo del worker (`WorkerProtocolVersion`);
- versión/build real de Navigatorr worker (`BuildGitCommit`, `WorkerVersion`);
- versión de FFmpeg;
- encoders disponibles;
- filtros `libvmaf` y `ssim`;
- profiles, pixel formats y opciones por encoder (`EncoderDetails`);
- registro de errores de probe estructurados (`ProbeErrors`) para fail-closed seguro;
- fingerprint determinista de capacidades (`CapabilityFingerprint`).

La respuesta general no debe fallar completamente porque un encoder o filtro particular esté ausente.

Semántica de knobs:

- valor omitido/`auto`: aplicar una omisión o fallback seguro documentado;
- valor solicitado explícitamente: rechazar si no está soportado;
- nunca ignorar silenciosamente una opción solicitada.

El modo benchmark debe usar un comando/protocolo remoto explícito que un worker antiguo rechace de forma segura. No debe poder confundirse con un transcode completo.

### 5.6 Sampling

Default inicial:

```yaml
sampling:
  strategy: distributed
  sample_count: 3
  sample_seconds: 20
  positions: [0.20, 0.50, 0.80]
```

Reglas:

- `positions` representa el centro deseado de cada sample.
- `start = position * duration - sample_seconds / 2`.
- Clampear el comienzo al rango válido.
- Evitar overlaps cuando la duración lo permita.
- Para videos cortos, reducir samples o usar el segmento completo sin exceder la duración.
- `sample_count: 1` y `sample_seconds: 60` cubre el caso simple de debugging; no agregar `single_sample_seconds`.
- Alinear timestamps, PTS, frame rate y número de frames entre referencia y candidato.
- No implementar detección de escenas, intros, créditos o black frames en la etapa inicial.
- Guardar outputs temporales solo bajo `state_dir/<job-id>/`.
- Eliminar únicamente archivos temporales conocidos de ese job.
- Persistir métricas y configuración, no archivos sample.

### 5.7 Candidatos VideoToolbox

Utilizar una lista explícita, ordenada y acotada de valores en lugar de asumir monotonicidad perfecta o implementar búsqueda binaria.

Ejemplo conceptual:

```yaml
search:
  max_candidates: 5
  quality_values: [60, 65, 70]
```

Reglas:

- máximo absoluto validado por schema (`DefaultMaxCandidates` = 5);
- ejecución secuencial dentro de un único benchmark job;
- misma selección de samples para todos los candidatos;
- parámetros completos serializados en cada resultado;
- no transcodificar audio, subtítulos o attachments durante la medición de video.

### 5.8 Métricas de calidad

Schema conceptual:

```yaml
quality:
  preferred_metric: vmaf
  vmaf:
    target: 96.0
    minimum: 95.0
    marginal_tolerance: 0.5
  ssim:
    target: 0.99
    minimum: 0.98
    marginal_tolerance: 0.005
```

Reglas:

- Comparar cada candidato con el mismo sample del original.
- Resolver de manera explícita pixel format, escala, timestamps y sincronización.
- Reportar cada score por sample y un agregado documentado.
- Para ser válido, el agregado debe superar el mínimo y ningún sample puede caer bajo el límite individual definido (per-sample quality gate).
- VMAF y SSIM tienen thresholds y tolerancias marginales independientes.
- Si la métrica preferida no existe, usar otra solo si dispone de thresholds configurados.
- Sin métrica válida se puede devolver un informe, pero no seleccionar automáticamente un ganador.
- No inventar scores ni convertir thresholds entre métricas.

### 5.9 Estimación de tamaño

Reportar por separado:

- `estimated_video_size_bytes`;
- `estimated_audio_size_bytes`;
- `estimated_subtitle_attachment_overhead_bytes`;
- `estimated_total_size_bytes`;
- `estimated_total_size_mb` (usando base decimal: 1,000,000 bytes);
- `estimated_savings_percent`;
- nivel o descripción de incertidumbre.

Reglas:

- estimar video desde los bytes/bitrate reales de video de los samples;
- estimar audio copiado con bitrate por stream cuando exista;
- usar fallback explícito y visible cuando ffprobe no proporcione bitrate;
- no multiplicar ciegamente el tamaño total del contenedor sample;
- tratar el overhead como una estimación conservadora, no como un valor exacto.

La orientación de tamaño se expresará en bitrate total:

```yaml
size:
  preferred_total_bitrate_kbps:
    min: 2100
    max: 3650
  soft_max_total_bitrate_kbps: 4250
```

Validado contra límite superior conservador (`MaxBitrateKbps = 1_000_000` kbps).

### 5.10 Selección y scoring

Aplicar reglas simples y explicables:

1. Rechazar candidatos inválidos, incompletos o con pérdida de streams críticos.
2. Rechazar candidatos bajo el mínimo de calidad (por muestra o por promedio).
3. Entre candidatos válidos, preferir el de menor tamaño que alcance el target.
4. Si ninguno alcanza el target pero algunos superan el mínimo, elegir el de mayor calidad y usar tamaño como desempate.
5. No pagar un aumento grande de bitrate por una mejora marginal sobre el target (usando tolerancia métrica específica).
6. Si ninguno supera el mínimo, no producir ganador automático.

El resultado debe incluir códigos de razón estables y mensajes legibles.

### 5.11 Actions

Agregar `benchmark_transcode` como action candidate-only que:

- comparte el preflight y los modelos con `transcode_media`;
- inspecciona, ejecuta samples y devuelve el informe;
- nunca ejecuta el episodio completo;
- nunca crea un candidato permanente;
- persiste el resultado en el estado de la action.

Extender `transcode_media` para que:

- mantenga exactamente el flujo actual cuando el perfil no activa optimización;
- ejecute el benchmark cuando `optimization.enabled=true`;
- use el plan ganador inmutable para el transcode completo;
- no continúe si no existe ganador válido;
- mantenga todas las validaciones y la aceptación candidate-only actuales.

### 5.12 Persistencia y observabilidad

Usar `state_json`, `outputs_json` y los logs de pasos existentes.

Persistir:

- source report relevante;
- recipe version/digest;
- capability fingerprint;
- samples y offsets;
- candidatos y parámetros exactos;
- métricas por sample y agregadas;
- tiempos de encoding;
- estimaciones;
- ganador y razones;
- plan/digest utilizado para el transcode final.

Limitar stdout/stderr de FFmpeg en outputs normales. Conservar diagnósticos acotados y habilitar detalle adicional solo en modo debug.

## 6. `profile=auto`

Conservar inicialmente las reglas actuales:

- clasificación de anime mediante metadata contextual;
- review para inspección no confiable, múltiples videos o streams incompatibles;
- no downscale automático;
- solo fuentes H.264 1080p sobredimensionadas;
- skip de fuentes HEVC, aunque sean grandes, durante la etapa inicial.

El flujo nuevo ampliará `auto` después de determinar que el archivo es candidato:

```text
auto eligibility existente
→ familia anime/general
→ recipe optimizada
→ capacidades
→ benchmark
→ ganador
```

No describir esta selección como clasificación visual del contenido.

## 7. Compatibilidad hacia atrás

Demostrado mediante tests que:

- recipes v1 siguen cargando;
- perfiles existentes sin `optimization` producen el mismo plan/argv relevante;
- `transcode_media` v2 conserva sus inputs y comportamiento candidate-only;
- `profile=auto` conserva las decisiones actuales fuera del nuevo camino optimizado;
- el worker rechaza de forma segura una petición de benchmark de protocolo no soportado;
- el `plan_digest` sigue siendo determinista;
- `replace_original=true` continúa rechazándose antes de cualquier submit.

## 8. Plan de trabajo paso a paso

### Fase 0 — Línea base y contrato
- [x] Confirmar working tree limpio y revisar el diff/rango base (`f8d5c3a`).
- [x] Ejecutar `go test ./...`.
- [x] Documentar fixtures y capacidades actuales.
- [x] Definir modelos JSON/YAML finales antes de implementar ejecución externa.
- [x] Añadir tests de compatibilidad que congelen el comportamiento v1 actual.

### Fase 1 — Inspección completa
- [x] Ampliar `DetailedStream`/`DetailedReport`.
- [x] Parsear video, color/HDR, frame rate y bitrates.
- [x] Parsear channel layout y bitrate de audio.
- [x] Mantener flags, tags, attachments y chapters.
- [x] Añadir fixtures/tests para H.264 8-bit, H.264 10-bit, HEVC Main10, HDR, múltiples audios, ASS/fonts y chapters.
- [x] Reutilizar el modelo en preflight, worker y validación.

### Fase 2 — Capacidades y protocolo
- [x] Definir `WorkerCapabilities` versionado con `ProtocolVersion == WorkerProtocolVersion`.
- [x] Detectar FFmpeg, encoders y filtros.
- [x] Reutilizar detección VideoToolbox mapeada canónicamente en `EncoderDetails`.
- [x] Añadir firma/fingerprint estable de capacidades.
- [x] Exponer capacidades mediante SSH al coordinador con tests de protocolo.
- [x] Reportar errores estructurados (`ProbeErrors`) y ausencias limpias por encoder específico.

### Fase 3 — Recipes v2
- [x] Parser v1/v2 compatible (`MinSchemaVersion`..`LatestSchemaVersion`).
- [x] Añadir `optimization` opcional a perfiles v2.
- [x] Validar sampling, quality, search y size con defaults aprobados y `MaxBitrateKbps`.
- [x] Omission safety en bloques métricos parciales (target/minimum nunca en cero silencioso).
- [x] Mantener strict decoding y límites máximos.
- [x] Actualizar mappings de overrides locales.

### Fase 4 — Sampling y workspace temporal
- [x] Implementar `SamplePlanner` puro y determinista en `transcode/optimization`.
- [x] Cubrir videos cortos, clamps, overlaps y límite `MaxSampleCount=32`.
- [x] **Fase 4A — Protocolo de benchmark, ciclo de vida persistente y workspace (auditoría y bloqueos corregidos)**:
  - [x] Modelos de benchmark públicos versionados con validación estricta (`BenchmarkRequest`, `BenchmarkCandidate`, `BenchmarkSampleWindow`, `BenchmarkStatus` sin exponer `RunToken` interno).
  - [x] Extensión de `Executor`/`SSHExecutor` (`BenchmarkSubmit`, `BenchmarkStatus`, `BenchmarkCancel`).
  - [x] Validación centralizada y endurecida de job ID (`ValidateBenchmarkJobID`: longitud 7..128, prefijo `bench-`, regex estricto, rechazo de path traversal `..`, separadores y nombres reservados de filesystem).
  - [x] Identidad de proceso estricta y protección contra PID reuse: secuencia exacta y contigua de argv `MatchesExactBenchmarkArgs` (`["_internal_benchmark", exactJobID, exactRunToken]`) verificado junto a PID vivo y start time en `IsBenchmarkExecutionAlive`.
  - [x] Cancelación fail-closed: solo señaliza si la identidad viva coincide inequívocamente; reconciliación segura a `cancelled` si ya murió o el PID es ajeno.
  - [x] Exclusión mutua de escritor único mediante file lock (`syscall.Flock` en `jobDir/.lock`) eliminando ventanas TOCTOU.
  - [x] Lock global de capacidad (`.capacity.lock` en `StateDir`): serialización atómica de comprobación de `MaxParallelJobs`, reserva y spawn entre benchmarks y transcodes con orden estricto `capacityLock -> jobLock` sin deadlocks.
  - [x] Idempotencia estricta en todos los estados: re-submit con mismo ID y mismo plan digest devuelve el estado actual sin spawnear nuevo proceso ni mutar tokens en `queued`, `running`, `completed`, `failed` o `cancelled`; para reintentar un job terminal se requiere un nuevo job ID; mismo ID con diferente digest se rechaza deterministamente como colisión.
  - [x] Limpieza post-spawn robusta: mata (`SIGKILL`) y recolecta (`Wait()`) el proceso si la persistencia de estado atómica falla tras `cmd.Start()`.
  - [x] Transiciones de estado monótonas por run (preservación de cancelación si llega durante la ejecución del runner).
  - [x] Aislamiento estricto de namespace y tipos: transcode `Submit` rechaza prefijo `bench-`; colisiones cruzadas (`job.json` vs `benchmark.json`) rechazadas; escaneo de active slots ignora dot-files y directorios no reconocidos.
  - [x] Workspace temporal `samples/` con cleanup seguro y acotado que jamás toca el source media ni el directorio raíz del job.
  - [x] Frontera inyectable `BenchmarkRunner` con fail-closed en producción (`"benchmark runner not implemented"`).
- [x] **Fase 4B — Extracción y encode FFmpeg de samples (auditoría adversarial y endurecimiento)**:
  - [x] Implementar extracción/encode de samples con argv seguro de FFmpeg en worker (`ProductionBenchmarkRunner`).
  - [x] Extracción de referencia sin pérdidas (`ffv1`, `-accurate_seek`, `-avoid_negative_ts make_zero`, video-only, `-an -sn -dn`).
  - [x] Encode secuencial y determinista de candidatos `hevc_videotoolbox` desde samples de referencia con validación previa de bit depth (8-bit vs 10-bit) y rechazo de HDR/DV.
  - [x] Estructuras de evidencia física (`BenchmarkExecutionEvidence`, `BenchmarkSampleRef`, `BenchmarkCandidateSampleResult`) persistidas en `benchmark.json`.
  - [x] Contrato de workspace seguro: verificación estricta de rutas hijas (`verifyChildPath`), sanitización de IDs y cleanup idempotente que nunca toca el medio original ni archivos fuera de `samples/`.
  - [x] **Seguridad de proceso y cancelación de process tree**:
    - Contexto de señal (`signal.NotifyContext` con `SIGTERM`/`SIGINT`) en `_internal_benchmark` y `_internal_run` (`main.go`).
    - Comandos FFmpeg aislados en su propio process group (`cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}`) con `cmd.Cancel = func() { syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }` y `WaitDelay = 2 * time.Second`.
    - `BenchmarkCancel` señaliza tanto el process group (`-record.PID`) como el PID directo con SIGTERM, verificando liveness tras 150ms antes de escalar a SIGKILL.
  - [x] **Endurecimiento TOCTOU y symlinks**:
    - Rechazo inmediato si `samplesDir` pre-existe como symlink (`os.Lstat`).
    - `prepareOutputFile` comprueba que `refPath` y `candPath` no pre-existan como symlinks (`os.Lstat`), y elimina archivos regulares pre-existentes antes de invocar FFmpeg para prevenir que `-y` siga symlinks.
    - `CleanBenchmarkSamples` rechaza si `jobDir` es un symlink, remueve únicamente la entrada symlink con `os.Remove` si `samplesDir` es symlink, y verifica mediante `filepath.EvalSymlinks` que el target resuelto permanezca estrictamente dentro de `jobDir` antes de ejecutar `os.RemoveAll`.
  - [x] **Nombres de archivo de candidatos libres de colisión**:
    - Clave inyectiva `candidateFileKey(candIdx, rawID, quality)` (`cand_<idx>_<sanitized>_<shortHash>_q<quality>_sample_<sampleIdx>.mkv`) combinando índice, etiqueta saneada, hash SHA-256 corto del ID original y calidad.
  - [x] **Preservación de evidencia parcial ante fallos o cancelación**:
    - `record.Evidence` se inicializa y enlaza inmediatamente al inicio de `RunBenchmark`.
    - Samples de referencia completados y resultados de encode de candidatos (incluyendo mensajes de error de candidatos fallidos) se preservan en `record.Evidence` y se persisten en `benchmark.json` al fallar el job.
    - En caso de cancelación por el coordinador mid-run (`latest.Status == "cancelled"`), la evidencia parcial en memoria se copia a `latest.Evidence` y se persiste atómicamente en `benchmark.json` sin alterar el estado `cancelled`, su timestamp de finalización ni su error, garantizando que la cancelación jamás resucite o mute el estado a `failed` ni deje muestras huérfanas en disco.
  - [x] **Captura de stderr acotada en memoria**:
    - Buffer acotado `boundedBuffer` con tope configurable (16 KB) y marcador `... [stderr truncated]` para prevenir fugas de RAM por logs verbosos de FFmpeg.
  - [x] **Detección exhaustiva de Dolby Vision, estabilidad de fuente y gating de chroma**:
    - Rechazo explícito de Dolby Vision en codecs (`dvh1`, `dvhe`, `dva1`, `dav1`, `dovi`), perfiles (`Dolby Vision`), tags de stream y side data (`DOVI configuration record`).
    - Snapshot de tamaño y modtime de la fuente (`os.Stat`), con verificación `verifySourceUnchanged` antes y después de cada extracción/encode, fallando cerrado si se modifica concurrentemente.
    - Gating de chroma subsampling: solo se permiten fuentes 4:2:0 de rango limitado (`yuv420p`, `nv12`, `yuv420p10le`, `p010le`); 4:4:4, 4:2:2 y fuentes de rango completo (`yuvj420p`) se rechazan fail-closed (el soporte de `yuvj420p` se difiere a la normalización de rango en la Fase 5 para prevenir inconsistencias de compresión de luma contra la referencia FFV1 en el scoring VMAF/SSIM).
  - [x] **Contrato documentado de alineación de frames y ventanas**:
    - Búsqueda exacta y rápida: `-accurate_seek -ss <startSec>` antes de `-i` localiza el keyframe previo y decodifica con precisión frame a frame hasta el timestamp. En fuentes VFR o con timebases irregulares, los límites de ventana solicitados están cuantizados/alineados al frame más cercano (frame-quantized/frame-aligned).
    - Ventana exacta: `-t <durationSec>`.
    - Normalización PTS: `-avoid_negative_ts make_zero` resetea la línea temporal a PTS 0.
    - Máster sin pérdidas: `-c:v ffv1` con `-an -sn -dn`.
    - Correspondencia frame a frame 1:1: El encode de candidatos consume el sample FFV1 extraído de principio a fin (frame 0 al final sin seeking ni recortes), garantizando emparejamiento idéntico de frames contra la referencia para scoring VMAF/SSIM en Fase 5.

### Fase 5 — Métricas y estimación
- [x] Modelos puros de evaluación VMAF con per-sample quality gate y agregación determinista en `transcode/optimization`.
- [x] Modelos puros de fallback SSIM con thresholds y tolerancias independientes.
- [x] Ineligibilidad explícita de HDR para scoring automático SDR.
- [x] Estimador de tamaño con cálculo de video por streams, preservación de audio copiado y reporte de incertidumbres.
- [x] Implementar cálculo remoto de filtros VMAF y SSIM con FFmpeg en worker (`internal/transcodeworker/benchmark_runner.go`):
  - Gating de capacidades y parser robusto: `ParseAvailableFilters` analiza de forma compatible tanto flags modernos de 2 caracteres (`.. libvmaf`, `TS ssim`) como legados de 3 caracteres (`..C`, `TSC`).
  - Gating estricto de 10-bit VMAF: fuentes de 10-bit con métrica VMAF o `both` fallan cerrado de inmediato con mensaje claro si el worker no tiene capacidad verificada de 10-bit VMAF, prohibiendo terminantemente la conversión silenciosa a 8-bit. El filtro SSIM nativo de FFmpeg procesa 10-bit (`yuv420p10le`) de forma nativa y segura sin pérdida de profundidad.
  - Generación de comandos segura y escape de filtergraph en dos niveles: `BuildVMAFArgs` y `BuildSSIMArgs` con escape exacto para `avfilter_graph_parse2` y `av_set_options_string` (`\\:` para `:`, `\\\'` para `'`, `\\\\` para `\`, `\\` para `[] ;,`) sin intermediación de shell, redirigiendo a null sink (`-f null -`).
  - Aislamiento en scratch: logs de métricas estructurados generados exclusivamente en `samples/` con esquema derivado libre de colisiones: `metric_<metric>_cand_<candIdx>_<hash>_sample_<sampleIdx>.<ext>`.
  - Hardening contra TOCTOU y symlinks: apertura a nivel de descriptor con `syscall.O_NOFOLLOW` (en Unix/macOS) y verificación de descriptor regular con `f.Stat().Mode().IsRegular()`.
  - Bounding de archivos y streaming: límite estricto de lectura a 5 MB (`MaxMetricLogSizeBytes`) implementado mediante `io.LimitReader(f, MaxMetricLogSizeBytes+1)`, fallando cerrado ante archivos vacíos o sobredimensionados.
  - Semántica de fallos a nivel de candidato: errores en la ejecución de métricas o parsing de logs de un candidato no abortan el benchmark completo; el candidato afectado se marca como inelegible (`Valid = false`, `IneligibleReason = ReasonIncompleteSampleScores`) y el benchmark continúa evaluando los candidatos restantes. Errores fatales a nivel de trabajo (cancelación, mutación de origen, corrupción de referencia) abortan de inmediato.
  - Parsing determinista y fail-closed: extracción de `pooled_metrics.vmaf.mean` con fallback a promedio de frames validando contigüidad estricta y sin huecos ni duplicados en `frameNum`; parsing de SSIM seleccionando el último resumen en stderr mediante un tail buffer acotado; rechazo de NaN, Inf y valores fuera de rango ([0, 100] VMAF, [0, 1] SSIM).
  - Soporte completo de `metric="both"`: persistencia y agregación determinista para ambas métricas (VMAF seguido de SSIM) por candidato en `CandidateMetrics`.
  - Preservación de medios e inmutabilidad: el source original, las referencias FFV1 y los candidatos HEVC nunca se modifican; verificación de inmutabilidad por hash SHA-256.
  - Orden determinista: ejecución secuencial estricta por candidato y por sample ($C \times S$).
  - Aislamiento de procesos y cancelación: ejecución en process group dedicado con propagación de SIGKILL y preservación de evidencia parcial.
  - Integración pura: reuso directo de `optimization.AggregateSampleScores` para cálculo de agregados de métricas tipados.
- [x] Cubrir ausencia de filtros, fallos y outputs incompletos en el worker con suite completa de pruebas unitarias e integrales en `benchmark_runner_test.go` y `capabilities_test.go`.


### Fase 6 — Búsqueda y selección VideoToolbox
- [x] Implementar `CandidateSelector` puro en `transcode/optimization/selector.go` con reglas multi-tier y códigos de razón estables.
- [x] Ejecutar evaluación y estimación de candidatos sobre los samples del benchmark en el worker (`internal/transcodeworker/benchmark_runner.go`).
- [x] Integrar `OutputEstimator` puro con metadatos reales del stream de video e inputs de fallback explícitos para audio y subtítulos.
- [x] Implementar selección determinista con resolución de políticas VMAF y SSIM (defaults aprobados y thresholds configurados), tolerancia marginal, y desempate por tamaño e ID.
- [x] Modelar y persistir `BenchmarkDecision`, `BenchmarkWinner` y lista completa de `BenchmarkCandidateEvaluation` preservando orden de candidatos en `BenchmarkExecutionEvidence` y `BenchmarkStatus`.
- [x] Garantizar decisiones explicables y tipadas de no-ganador (`ReasonNoCandidateMetMinimumQuality`, `ReasonAllCandidatesInvalid`, `ReasonUnusableEstimate`) sin fabricar ganadores ni scores.
- [x] Validar fallbacks y configuraciones de calidad fail-closed en `ValidateBenchmarkRequest`.
- [x] Verificar determinismo estricto, tolerancia a fallos por candidato, preservación de evidencia en cancelación/fallo, e invariancia ante permutaciones del orden de candidatos con suite exhaustiva de tests unitarios e integrales en `benchmark_runner_selection_test.go`.

### Fase 7 — Actions e integración
- [x] Registrar `benchmark_transcode`.
- [x] Añadir lifecycle persistente de submit/wait/result/cancel.
- [x] Integrar la ruta optimizada en `transcode_media` sin alterar la ruta legacy.
- [x] Crear el plan ganador inmutable y su digest.
- [x] Mantener validación completa y aceptación candidate-only.
- [x] Verificar resume/idempotencia y `worker_busy`.
- [x] Confirmar compatibilidad básica con `transcode_batch`.

### Fase 8 — Documentación y validación real
- [ ] Actualizar `docs/TRANSCODING.md`.
- [ ] Actualizar recipe embebida y ejemplos sin cambiar perfiles existentes.
- [ ] Documentar sampling, métricas, FAST, bit depth y troubleshooting.
- [ ] Ejecutar suite completa uncached (`go test -count=1 ./...`).
- [ ] Compilar el worker desde el commit actual.
- [ ] Verificar protocolo y capacidades reales en el M1 Max.
- [ ] Ejecutar benchmark-only sobre un archivo real autorizado.
- [ ] Ejecutar transcode completo solo si el benchmark es válido.
- [ ] Mantener `replace_original=false` y verificar SHA-256 antes/después.
- [ ] Abrir el PR único hacia `main` sin merge.

## 9. Estado de avance

| Fase | Estado | Evidencia | Commits aceptados |
| --- | --- | --- | --- |
| 0. Línea base y contrato | Completo | Contrato v1 protegido por tests, suite completa en verde, modelos v2 acordados | `eaadfe1`, `94a5a01`, `5eb1d30`, `9e90d7b` |
| 1. Inspección completa | Completo | `DetailedReport`/`DetailedStream` extendido (color space/primaries/transfer/range, HDR/mastering metadata, frame rate racional y calculado, bitrates numéricamente acotados, channel layout de audio, side data), fixtures H264 8-bit/10-bit, HEVC Main10, HDR BT.2020, chapters y subtítulos | `eaadfe1`, `94a5a01`, `5eb1d30`, `9e90d7b` |
| 2. Capacidades y protocolo | Completo | `WorkerCapabilities` versionado (`ProtocolVersion == WorkerProtocolVersion`), probe errors estructurados, clean absence encoder-specific, eliminación de campo redundante `VideoToolbox`, fingerprint determinista de capacidades, handshake SSH | `eaadfe1`, `94a5a01`, `5eb1d30`, `9e90d7b` |
| 3. Recipes v2 | Completo | Loader v1/v2 compatible (`MinSchemaVersion`..`LatestSchemaVersion`), `OptimizationPolicy` validado con defaults aprobados (VMAF 96/95/0.5, SSIM 0.99/0.98/0.005, sampling bounds 1..32, `MaxBitrateKbps = 1_000_000`), omission safety en bloques métricos parciales | `eaadfe1`, `94a5a01`, `5eb1d30`, `9e90d7b`, `c92725b` |
| 4. Sampling y temporales | Completo | Fase 4A (protocolo, SSH, models, locking `.capacity.lock` y `jobDir/.lock`, argv exacto `MatchesExactBenchmarkArgs`, idempotencia total) y Fase 4B (`ProductionBenchmarkRunner`, extracción `ffv1`, encode `hevc_videotoolbox`, `verifyChildPath`, bit depth gating, evidencia `BenchmarkExecutionEvidence`, cleanup acotado y seguro, process group cancellation, symlink TOCTOU hardening, collision-free candidate names, partial evidence preservation on error and cancel, bounded stderr, DV y chroma 4:2:0 gating, deferral de full-range `yuvj420p`) completas y verificadas | `e23f204`, `8fe5828`, `badad4a`, `7fb5108`, `a35bcd6`, `f57a8af`, `35d9682`, `5c69e0d`, `76dba67` |
| 5. Métricas y estimación | Completo | Modelos puros (`transcode/optimization/metrics.go`, `estimator.go`) y runner remoto FFmpeg (`internal/transcodeworker/benchmark_runner.go`) con libvmaf/ssim filter capability gating (soporte 2 y 3 caracteres), filtergraph path escaping en dos niveles, parsing robusto con bounding 5MB vía LimitReader y O_NOFOLLOW, candidate-level failure isolation, 10-bit VMAF fail-closed gating, frame contiguity verification, SSIM last-match tail parsing, metric='both' dual aggregates, inmutabilidad de medios y agregación tipada pura antes de limpieza de scratch | `e23f204`, `8fe5828`, `badad4a`, `3eade67`, `7651b64` |
| 6. Selección VideoToolbox | Completo | Pipeline de selección y estimación integrado en `ProductionBenchmarkRunner` conectando `transcode/optimization` con evidencia de Fase 4B/5; modelos tipados `BenchmarkDecision`/`BenchmarkWinner`/`BenchmarkCandidateEvaluation` persistidos en evidencia y status; soporte para métricas `vmaf`, `ssim` y `both` con fallback determinista; validación fail-closed de policies y fallbacks; suite completa de 10 tests de selección en `benchmark_runner_selection_test.go` | `e23f204`, `8fe5828`, `badad4a`, `e0250ce`, `f96907b` |
| 7. Actions e integración | Completo | Action `benchmark_transcode` y pasos `submit_benchmark`/`wait_benchmark` en `transcode_media`; resolución de recipes v2 con `ResolveProfile`; plan ganador inmutable con `PlanDigest`; gating fail-closed (SDR, bit depth, HDR/DV, capabilities); propagación de cancelación y `WorkerBusy`; 13 tests unitarios e integrales en `transcode_benchmark_test.go` | `118d2b4` |
| 8. Validación y PR | Pendiente | Validación en M1 Max, benchmarks reales y apertura del PR único hacia `main` pendientes. | — |

### Detalle de commits aceptados en el worktree

- **Fundación (Fases 0–3)**:
  - `eaadfe1`: `feat(transcode): implement phases 1-3 optimization foundation`
  - `94a5a01`: `fix(transcode): address review corrections for inspection, capabilities, and recipe schema v2`
  - `5eb1d30`: `fix(transcode): address coordinator review round 2 corrections for phases 1-3`
  - `9e90d7b`: `fix(transcode): address final foundation review for phases 1-3`
- **Núcleo puro de optimización (Fases 4–5 puro)** *(cherry-picked desde `agent/task_1789319542185_89d960ea`)*:
  - `e23f204` *(origen `83479dfffd7b429f55f524898a3139f8c2360e81`)*: `feat(transcode): add pure Go optimization core package`
  - `8fe5828` *(origen `a26d5f42a3576cc9c01da3ad702c9e0443632ca3`)*: `fix(transcode/optimization): address coordinator review for quality gate and estimator robustness`
  - `badad4a` *(origen `b0362095d2236e5100b3dbd693655cd18578c570`)*: `transcode/optimization: harden metric validation, bounded sampling, audio estimation, and overflow guards`
- **Alineación de contratos y adaptadores**:
  - `c92725b`: `test(transcode): verify recipe schema v2 alignment with pure optimization-core contracts`
- **Protocolo y ciclo de vida de benchmark (Fase 4A)**:
  - `7fb5108`: `feat(transcode): implement phase 4A benchmark protocol and persistent worker lifecycle`
  - `68297aa`: `docs(transcode): record completion of phase 4A benchmark protocol and worker lifecycle`
  - `a35bcd6`: `fix(transcode): harden phase 4A execution identity, idempotency locking, and namespace isolation`
  - `7249e93`: `docs(transcode): record Phase 4A audit corrections and contracts`
  - `f57a8af`: `fix(transcode): enforce exact benchmark argv sequence, global capacity lock, and strict submit idempotency`
  - `20cd037`: `docs(transcode): record Phase 4A blocker corrections and capacity locking contracts`
- **Extracción y encode FFmpeg de samples (Fase 4B)**:
  - `35d9682`: `feat(transcode): implement phase 4B sample extraction and candidate encoding runner`
  - `0534336`: `docs(transcode): record completion of Phase 4B sample extraction and candidate encoding runner`
  - `5c69e0d`: `fix(transcode): harden phase 4B cancellation, symlink toctou, filename collision, and evidence tracking`
  - `532d9dd`: `docs(transcode): record Phase 4B adversarial corrections and process group safety`
  - `76dba67`: `fix(transcode): persist partial evidence on cancel and reject full-range yuvj420p`
- **Medición remota VMAF y SSIM (Fase 5)**:
  - `3eade67`: `feat(transcode): implement Phase 5 remote VMAF and SSIM measurement`
  - `7651b64`: `fix(transcode): address Phase 5 adversarial review corrections`
- **Evaluación de candidatos y selección determinista (Fase 6)**:
  - `e0250ce`: `feat(transcode): implement Phase 6 candidate evaluation and deterministic selection`
  - `f96907b`: `fix(transcode): fail closed on invalid preferred_metric in benchmark quality config`
- **Actions e integración (Fase 7)**:
  - `118d2b4`: `feat(action): implement phase 7 transcode benchmark and optimization integration`
