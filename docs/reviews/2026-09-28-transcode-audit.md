# Revisión del informe de transcodificación

Verificado el 28 de septiembre de 2026 contra el código `481610f`, las acciones
citadas y los perfiles efectivos consultados por MCP. Las secciones iniciales
registran el diagnóstico previo; las correcciones y pruebas posteriores aparecen
al final.

## Conclusión principal

No se confirmó el supuesto error de ordenación x265/VideoToolbox. El selector
compara bytes, pero primero limita la pérdida respecto al mejor score mediante
`marginal_tolerance`. El nombre del resultado omite esa condición y confunde.

La petición persistida de `act-benchmark-transcode-6136633732386131` tenía
target 92, minimum 88 y tolerancia 0,5. El mejor score era 95,6818: el umbral
efectivo para competir por tamaño era **95,1818**.

| Candidato | Score | MB estimados | Dentro del margen del mejor |
|---|---:|---:|---|
| CRF20 | 95,6818 | 252,03 | Sí |
| CRF22 | 94,8815 | 214,10 | No |
| CRF24 | 93,8003 | 186,14 | No |
| CRF26 | 92,5063 | 165,34 | No |

Escoger CRF20 coincide con el contrato actual. Escoger CRF26 requeriría cambiar
la política, no arreglar una comparación. `eligible` y `target_reached` no
significan que el candidato pertenezca al grupo final que compite por tamaño.
Con VideoToolbox, los scores citados 97,0733 y 97,4688 distan menos de 0,5;
ambos entran en ese grupo y gana Q70 por tamaño. No hay contradicción.

Código: `transcode/optimization/selector.go:140`, con pruebas existentes
`TestCandidateSelector_TargetReachedWithinTolerance` y `...OutsideTolerance`.

## Contraste de los quince puntos

| Punto del informe | Resultado de la revisión | Corrección o siguiente paso |
|---|---|---|
| 1. Ganador x265 incorrecto | Diagnóstico descartado; mensaje incompleto confirmado. | Exponer mejor score, tolerancia, umbral efectivo y por qué un candidato válido no ganó. Conservar la selección actual mientras no se decida cambiar su política. |
| 2. Main10 sin guardrails equivalentes | Confirmado en `anime-x265-medium-main10`, managed generación 2. Solo tiene VMAF 92/88 y tolerancia 0,5. | Preparar y validar una actualización del perfil con modelo explícito, p5, peor ventana, CAMBI y validación final. Cambiar a VMAF v1 cambia el contrato de medición; no tratar los scores viejos como validación del nuevo perfil. |
| 3. Rechazo por sample poco visible | Confirmado en el código del resumen: el rechazo genérico no aporta sample/valor/umbral. | Proyectar evidencia por sample con timestamp, valor y límite; ya se muestran valores y límites para varios guardrails avanzados. No inventar estos datos cuando el histórico no los conserva. |
| 4. CAMBI no distingue banding de origen | Esa premisa es incorrecta: Hikaru pidió `full_ref` y el worker lee `cambi_full_reference`. | Inspeccionar el fragmento de 1105–1106 s y, si hace falta, exponer source/distorted además del delta ya existente. Un pico estable no demuestra por sí solo que falle la métrica ni justifica relajar límites. |
| 5. Encadenamiento de benchmarks | Ya corregido y con regresiones de revisión explícita, nueva acción/reinicio, cancelación y codificación directa. | Mantener esas pruebas; no crear otro mecanismo de búsqueda. |
| 6. Históricos sin outcome | No requiere backfill para `action_status`: se calcula al leer. Se verificó en el benchmark antiguo de Hikaru. `action_detail` devuelve el estado persistido. | Aclarar qué herramienta consultar y sus contratos; no reescribir acciones históricas para duplicar una vista derivada. |
| 7. VMAF de 10 bits | Corregido; regresiones permanentes en `benchmark_native10_test.go`. | Conservar la prueba del pipeline y el rechazo sin capacidad. La prueba real depende de FFmpeg/libvmaf disponibles. |
| 8. Política compact opaca | La política existe en `transcode/audio.go` y está documentada en `docs/TRANSCODING.md`; no es suficientemente descubrible desde el schema MCP. | Añadir descripción/consulta de política, reutilizando la implementación existente. No hace falta añadir knobs ni otro optimizador. |
| 9. Fallback de audio ambiguo | Es un dato de estimación, no una orden de conversión. `planned_audio` ya corrige la lectura operativa. | Describirlo explícitamente como respaldo del estimador; mantener compatibilidad del campo. |
| 10. Sin validación final de audio | Parcialmente incorrecto: ya se comprueban codec, cantidad de pistas, idioma conocido y canales, antes de publicar el candidato. | Faltan comprobaciones específicas de disposiciones de audio y tiempo por pista. La comprobación actual de duración es del archivo, no de cada audio. Evaluar bitrate con tolerancia, no exigir una igualdad exacta al objetivo AAC. |
| 11. Overrides ocultando bundle | Confirmado: 15 managed, precedencia managed > static_config > active_bundle y nombres coincidentes. | Mostrar qué perfil base está oculto y avisar de diferencias. No introducir herencia/patches globales sin necesidad. Una actualización del bundle no cambia un override completo. |
| 12. recovery_verified confuso | Confirmado: finalize lo vuelve false después de limpiar el backup. La promoción exitosa consultada conserva la evidencia de integridad en otros campos. | Distinguir verificación histórica de estado actual del backup. No inferir presencia física solo del booleano persistido. |
| 13. Protección ante pérdida de adopción | La comprobación existe justo antes de eliminar y hay pruebas relacionadas de identidad/archivo alterado. | Añadir un caso explícito de pérdida de adopción después del import si no está cubierto por el escenario exacto; conservar recovery. No se repitió una promoción real destructiva durante la revisión. |
| 14. Diagnostics inconsistente | Hay un bug real adicional: consulta mantenimiento con status `open`, que `ValidMaintStatus` rechaza; ignora el error y muestra cero. También llama active a running y limita conteos a 100. | Contar estados definidos con consultas de conteo, reportar errores y distinguir running, waiting y pendientes/no finalizados. |
| 15. Batch v5 sin E2E real | Hay pruebas de calibración compartida, límites, recetas inmutables y candidatos. No se verificó aquí una temporada real completa con promoción. | Validación controlada de pocos elementos y aprobación de promoción. No es necesario promover una temporada entera para verificar inicialmente el flujo. |

## Audio compact: política actual comprobada

DTS, TrueHD, MLP, FLAC, ALAC y PCM se convierten a AAC: 128 kb/s mono,
192 estéreo, 384 para 3–6 canales y 512 para 7–8 canales. AAC, AC3, EAC3,
Opus y otros codecs se copian; también se copian los de número de canales
desconocido o superior a ocho. Se conservan pistas, idiomas y canales;
la conversión a AAC es con pérdida y esos invariantes no prueban transparencia
auditiva. No hay codificación de audio durante el benchmark.

## CAMBI

Netflix documenta el modo full-reference como diferencia no negativa entre el
score del candidato y el de la referencia. Por tanto añadir otro baseline/delta
como requisito supondría duplicar el mecanismo existente:
[documentación de CAMBI](https://github.com/Netflix/vmaf/blob/master/resource/doc/cambi.md).

El resumen actual de Hikaru ya presenta CAMBI medio/pico, límites 6/16 y la
posición a revisar. La comparación visual del fragmento sigue pendiente; los
números por sí solos no resuelven si el umbral resulta adecuado para ese material.

## Orden propuesto, sin ampliar arquitectura

1. Hacer explícita la condición de tolerancia del selector y corregir los
   contadores falsos de mantenimiento. Son correcciones acotadas de contrato.
2. Completar las verificaciones de audio y preparar el perfil Main10 con
   guardrails y validación final. Preservar la opción de codificación directa.
3. Mejorar evidencia por sample, descubrimiento de compact y avisos de override;
   aclarar recovery sin cambiar el protocolo seguro de promoción.
4. Probar el batch con pocos elementos representativos. Las estimaciones de
   tamaño de muestras no son tamaños finales ni intervalos de confianza.

La comparación x265/VideoToolbox usa objetivos de calidad diferentes (92 frente
a los objetivos más altos de VideoToolbox). No demuestra por sí sola superioridad
a igual calidad. Comparar los mismos modelos, muestras y guardrails antes de
convertir ese resultado en una recomendación general.

## Validación realizada

Lectura de las acciones y perfiles citados por MCP, revisión de selector,
estimador, resumen, audio, promoción y conteos. Pruebas focalizadas aprobadas
en `transcode/optimization`, `action` e `internal/transcodeworker`; el filtro
no seleccionó pruebas en `tools`. Log: `/private/tmp/navigatorr-document-audit-tests.log`.
No se ejecutó una nueva transcodificación ni una promoción, y no se alteraron
los perfiles activos.

## Correcciones posteriores autorizadas

- Selección: se conserva la política y se publican mejor score, tolerancia,
  umbral y candidatos fuera del margen. Regresión con los cuatro resultados
  citados de Eminence y orden de entrada variable.
- Muestras rechazadas: decisiones nuevas conservan timestamp, valor y mínimo;
  los históricos solo reconstruyen datos respaldados por su petición persistida.
- Audio: política visible desde `recipe_get`, disposiciones explícitas y
  validación por paquetes de duración, desfase A/V y bitrate excesivo. No se
  interpreta el bitrate nominal como una demostración de calidad auditiva.
- Recetas: aviso de overrides completos sobre static/bundle y digest de la base.
- Recuperación: evidencia histórica separada de limpieza completada; permanece
  la comprobación de adopción inmediatamente anterior a eliminar el original.
- Diagnostics: conteos SQL sin límite de página, estados definidos y errores de
  base de datos visibles como degradación, nunca como cero trabajos.
- Perfil Main10 propuesto y revisable en `docs/examples/`: mantiene 92/88 y
  tolerancia 0,5, agrega VMAF v1, p5 84, peor ventana 85/5 s, CAMBI 6/16
  full-reference y validación final sampled. No se relajan límites.

Verificación previa al merge: suite Go completa, `go vet`, detector de carreras
en regresiones de estado/recetas/conteos, y pruebas nativas ejecutadas en el M1.
Las pruebas reales cubren VMAF Main10 legacy/combinado/v1+CAMBI y dos pistas
FLAC→AAC con Main10, pista predeterminada preservada y rechazo de audio truncado
o desplazado. El lote integrado de dos elementos verifica calibración compartida,
validación de candidatos, aprobación única, promoción, limpieza y reanudación.
Este último usa Sonarr aislado y evidencia determinista del worker: no equivale
a promover una temporada real en producción. También se conserva la prueba de
codificación directa sin capacidad VMAF.

Prueba de perfil sobre Eminence S01E05: acción
`act-benchmark-transcode-3162383861356162`, fuente
`42e89feaac584cc848618e830af53292cb6ad98088aad84f71b742bec7996935`.
Es un benchmark de muestras y no autoriza por sí mismo a sustituir el episodio.
