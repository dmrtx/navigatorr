# Perfil Joy: experimento con muestras

Los archivos Joy existentes de The Grand Tour T3E09, T3E12 y T3E14 conservan
metadatos de x265: Main10, ABR de 1.200 kb/s, estadísticas de una pasada previa,
lookahead 80 y HE-AAC 5.1 de aproximadamente 162–190 kb/s. La variante toma
como referencia los ajustes principales del T3E12: seis B-frames, cuatro
referencias, AQ modo 1/fuerza 0,8, psy-rd 0, psy-rdoq 1 y ssim-rd activado.
No promete reproducir byte a byte x265 2.9 ni el encoder de audio histórico.

- `examples/transcode-profile-joy.json`: referencia experimental de 1.200 kb/s.
- `examples/transcode-profile-joy-1600.json`: variante de 1.600 kb/s, con el resto
  de ajustes y los límites de calidad idénticos.

Ambas usan x265 `slow` en dos pasadas, Main10, conservación de pistas y
metadatos, y audio `he_aac`. Esta política de audio convierte explícitamente
pistas de uno/dos canales a 64 kb/s y de tres a seis canales a 160 kb/s mediante
AudioToolbox. Conserva cantidad de pistas, canales, idioma y disposiciones;
copia pistas con cantidad desconocida o superior a seis canales. La conversión
es con pérdida y no conserva metadatos de audio por objetos.

El nuevo objeto tipado `video.x265` mantiene la configuración dentro de la
identidad inmutable del perfil, la solicitud, el ganador y el plan. Los perfiles
anteriores siguen con su ruta CRF/VideoToolbox. Cada muestra/codificación posee
estadísticas privadas; un error o cancelación en la primera pasada impide la
segunda. La fase `analysing_video` distingue el análisis de la codificación.

La activación requiere coordinador y worker con las capacidades
`x265_two_pass` y `he_aac_audio`. Un worker anterior se rechaza antes de enviar
el trabajo. Estos archivos son propuestas explícitas y no se incorporan a la
selección automática ni cambian el perfil predeterminado. No deben escribirse
en el registro gestionado de un coordinador anterior, que no conoce el esquema.
Para una fuente de ocho bits, un benchmark Main10 requiere la decisión explícita
`preserve_source_bit_depth=false`; no recupera información ausente en la fuente.

## Validación real del 16 de octubre de 2026

Fuente: The Grand Tour T3E01, edición QOQ, H.264 1080p de ocho bits y E-AC3 5.1.
Se copiaron pistas de tres fragmentos alrededor del 20 %, 50 % y 80 % del
episodio; el inicio puede incluir el fotograma clave anterior. Los fragmentos
duran 24,12, 23,00 y 20,60 s. No son un muestreo exhaustivo del episodio.

La ejecución se realizó en un MacBook Air Apple Silicon, FFmpeg 9.0.1 y una
copia aislada de libvmaf 3.2.0 del worker; biblioteca SHA-256
`f9ca4510cb08cca61ecd571fc417f2b4440c25555fbeefdb4b5477a35891de1c`.
No se modificó la instalación local de FFmpeg, el worker, sus procesos ni su
cola. No se generó una codificación completa ni se publicó/reemplazó contenido.

Límites fijados antes de codificar: VMAF v1 de media mínima 93 y objetivo 95;
percentil 5 mínimo 88; peor ventana de cinco segundos mínimo 90; CAMBI
full-reference de media máxima 6 y pico máximo 16. Se exige que cada fragmento
cumpla los mínimos. Las métricas usan diez bits y la profundidad real del
candidato; no se reduce la precisión de medida a ocho bits.

### Referencia de 1.200 kb/s

| Muestra | VMAF medio | P5 | Peor 5 s | CAMBI medio/pico | Aceptada |
| --- | ---: | ---: | ---: | ---: | --- |
| 1 | 92,734 | 87,291 | 90,696 | 0 / 0 | No |
| 2 | 92,974 | 88,562 | 91,278 | 0,007 / 1,714 | No |
| 3 | 91,665 | 87,922 | 90,838 | 0 / 0 | No |

VMAF combinado 92,489; ahorro medido del 81,1–83,9 % en los fragmentos.
Los tres candidatos verificaron Main10 1920×1080, HE-AAC 5.1, estructura,
temporización de audio, decodificación y hashes del fragmento original sin
cambios. Esto no sustituye una revisión visual o una escucha humana.
Evidencia: `reviews/fixtures/joy-1200-samples-2026-10-16.json`.

### Variante de 1.600 kb/s

| Muestra | VMAF medio | P5 | Peor 5 s | CAMBI medio/pico | Aceptada |
| --- | ---: | ---: | ---: | ---: | --- |
| 1 | 94,195 | 89,567 | 92,001 | 0 / 0 | Sí |
| 2 | 95,047 | 90,702 | 93,193 | 0,001 / 0,172 | Sí |
| 3 | 93,701 | 90,839 | 92,718 | 0 / 0 | Sí |

VMAF combinado **94,334**; ahorro medido del **75,5–79,0 %** en los fragmentos.
Cumple los mínimos en las tres muestras, aunque el objetivo 95 no se alcanza en
cada una. Main10, HE-AAC 5.1, temporización, decodificación y hashes se verificaron
en las tres. También coinciden los paquetes de vídeo de fuente y candidato:
596, 569 y 511 por muestra, en ambas variantes. Las ubicaciones de las métricas
en la evidencia versionada se expresan en segundos locales del fragmento.
Evidencia: `reviews/fixtures/joy-1600-samples-2026-10-16.json`.

La variante de 1.600 kb/s es la propuesta para la siguiente fase. El experimento
no la instaló en el registro de producción ni actualizó/reinició servicios:
el soporte y los perfiles quedan preparados en el repositorio. No se autorizó
una codificación completa ni un reemplazo a partir de esta prueba.

## Reproducción aislada

El test optativo exige un directorio con `joy-source-0.mkv` a
`joy-source-2.mkv` y un directorio nuevo para resultados. Nunca carga el estado
ni la configuración del worker de producción. La máquina necesita x265,
AudioToolbox HE-AAC y VMAF v1/CAMBI realmente ejecutables.

```sh
NAVIGATORR_JOY_SAMPLES_DIR=/ruta/fragmentos \
NAVIGATORR_JOY_RESULT_DIR=/ruta/nueva/resultados \
NAVIGATORR_JOY_PROFILE_FILE=/ruta/transcode-profile-joy-1600.json \
go test ./internal/transcodeworker -run '^TestJoyRealSourceSamples$' -count=1 -v
```

`report.json` separa ejecución técnica y aceptación de calidad. Un test técnico
correcto puede producir `quality_accepted=false`; el resultado debe leerse.
Los fragmentos conservan hashes propios y sus candidatos no salen del directorio
aislado. Las estadísticas ABR pertenecen a cada fragmento: el reparto de bits de
dos pasadas sobre un episodio completo será diferente. La aceptación de muestras
solo permite considerar la siguiente etapa; un candidato completo seguirá
requiriendo validación final y una decisión explícita de reemplazo.
