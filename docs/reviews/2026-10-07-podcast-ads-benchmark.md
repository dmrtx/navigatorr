# Issue #93: prueba real de eliminación de anuncios

Este documento conserva las medidas del experimento previo al pipeline durable.
La implementación posterior y sus controles actuales se describen en
[Podcast cleaning](../podcast-cleaning.md). Las medidas de clasificación aquí
no certifican la integración de producción con MoonStation.

## Resultado

La transcripción local y el recorte son suficientemente rápidos para continuar
con el proyecto. La clasificación detectó los cinco rangos comerciales, pero
los límites todavía necesitan revisión: low dejó una palabra de un anuncio y
high dejó sus primeros 8,46 segundos. Este experimento no justifica habilitar
publicación automática.

Se creó un harness experimental en `scripts/podcast-benchmark/`. Durante ese
experimento, la acción
`clean_podcast_ads`, la integración durable con el worker/Action Engine, UI/MCP
y MoonStation aún no estaban implementadas. En ese experimento no se desplegó
ni publicó nada.

## Episodio y entorno

- Generation Why #700, **1984 San Ysidro McDonald's Massacre**.
- [Feed público](https://rss.art19.com/generation-why-podcast), episodio del
  20 de septiembre de 2026; copia descargada el 7 de octubre.
- El feed indica 47:55; la copia realmente descargada mide 51:33,708.
  Todos los cálculos usan el archivo medido, no la duración del feed.
- Source: 49.614.725 bytes, MP3 estéreo, 44.100 Hz.
- SHA-256: `caeb7f548a323e83d0c7891161a7eb7c05dcdc4e931e0d9fda371a0d036d3cf4`.
- ASR y render reales en **Apple M1 Max**, macOS 26.6.2, FFmpeg 9.0.2.
  La coordinación y pruebas locales corrieron en otro Mac, Apple M4.
- El episodio permite comprobar la diferencia entre publicidad y menciones
  narrativas a una marca: McDonald's forma parte del caso.

## Medidas

Apple SpeechAnalyzer produjo **8.209 unidades de una palabra**, todas con timing
nativo. Tiempo de análisis/finalización: **54,460 s**. Realtime factor:
**0,01760**, aproximadamente **56,8 veces tiempo real**. El tiempo excluye
descarga, transferencia, hashing y preparación de assets de idioma.

Luna se ejecutó mediante subagentes independientes con el mismo transcript y
política. Cada uno leyó los 15 bloques por separado, con ventanas de cuatro
minutos y aproximadamente 30 segundos de overlap. No hubo truncamiento en la
comparación definitiva. Los tiempos incluyen herramientas y escritura de
artefactos del agente; no son latencias de una llamada API.

| Medida | Luna low | Luna high | Copia revisada a partir de low |
| --- | ---: | ---: | ---: |
| Clasificación del agente | 152,145 s | 147,309 s | Reutilizada |
| Bloques leídos / clasificados | 15 / 15 | 15 / 15 | Reutilizados |
| Unidades explícitamente clasificadas | 100% | 100% | 100% |
| Conflictos en overlap / uncertain declarado | 0 / 0 | 0 / 0 | 0 / 0 |
| Rangos de corte | 5 | 5 | 5 |
| Duración eliminada | 115,560 s | 108,360 s | 116,820 s |
| Duración final | 49:38,148 | 49:45,348 | 49:36,888 |
| Render + preparación + validación | 19,827 s | 19,834 s | 19,775 s |
| Decodificación completa y duración | Pasa | Pasa | Pasa |

La suma ASR + agente + render/validación es 226,43 s para low y 221,60 s para
high. No representa un tiempo end-to-end observado sin intervención: excluye
descargas, transferencias, coordinación, revisión y los intentos descartados.
Una ejecución por configuración no determina diferencias de velocidad
significativas. No hay datos de tokens facturados ni coste real.

## Calidad observada

La política elimina `paid_ad`, incluidas ofertas comerciales de suscripción a
Audible. Conserva `house_promo`, `cross_promo` y `content`. Por ello los tráileres
de otros podcasts y los pedidos de Patreon/reseñas siguen en las copias.

Ambos resultados definitivos conservan la discusión sobre McDonald's, las
recomendaciones del documental y los créditos del programa. No hay cortes
etiquetados como publicidad en el cuerpo narrativo del caso.

En el anuncio de Audible del medio, la oración comercial empieza en
`u002813`, a **17:22,380**. Low inicia el corte en `u002814`, a 17:23,640,
dejando la palabra inicial «You» durante 1,26 s. High empieza en `u002837`, a
17:30,840, dejando 8,46 s de la introducción comercial. No lo marca como
incierto. Un razonamiento más alto no corrigió este límite en esta prueba.

La copia revisada ajusta solamente ese inicio a `u002813`, mediante revisión
semántica del transcript por el agente principal. Se conservaron los resultados
raw originales. Esto no es una referencia humana independiente ni una
certificación de los límites por escucha.

Un primer ensayo fue descartado para la comparación principal porque low no
había observado toda la salida truncada. También había mezclado el tráiler de
My Mom's Murder con las ofertas comerciales adyacentes. La segunda prueba usó
lecturas separadas y una regla explícita para separar el tráiler de la oferta.
Esto muestra por qué declarar cobertura en JSON no basta para demostrar que el
LLM realmente recibió/leyó el texto.

No se midieron precision/recall contra un conjunto humano de referencia. La
cobertura es sobre el transcript reconocido, no sobre cada segundo de audio.
Hay dos gaps de timing de 39,06 y 61,44 s, que el planner conserva. Los agentes
retienen contexto entre bloques; no son llamadas API independientes por bloque.

## Recorte y comprobaciones

El renderer usa los tiempos nativos, sin silencio snapping, y crea una copia
MP3 VBR q2. Los cinco cortes revisados son:

| Inicio en el original | Fin | Duración |
| --- | --- | ---: |
| 00:00,000 | 00:07,920 | 7,92 s |
| 00:57,900 | 01:27,840 | 29,94 s |
| 17:22,380 | 18:23,460 | 61,08 s |
| 19:11,040 | 19:23,760 | 12,72 s |
| 51:28,200 | 51:33,360 | 5,16 s |

El primer render falló cerrado con `inadequate AVFrame plane padding` de
libmp3lame. Se corrigió el empaquetado de frames después del concat mediante
`asetnsamples=n=1152:p=0`, sin agregar silencio. Se reintentó solo render y
validación, reutilizando transcript y clasificación. Las tres copias pasaron
ffprobe, la tolerancia de duración de 250 ms y la decodificación completa con
FFmpeg. El hash del original se verificó antes y después; también se verificaron
los hashes de las copias transferidas al proyecto local.

**18 pruebas** Python pasan, incluyendo gaps, IDs inventados, rangos fuera del
bloque, overlaps contradictorios, incertidumbre, cambios de source/transcript,
rechazo de sobrescritura, recorte real estéreo no alineado y una copia exacta
de MP3 sin anuncios. El ejecutable Swift compiló para macOS 26 y ejecutó ASR
real. En este experimento el código Go de producción no se cambió ni se
ejecutó su suite; las pruebas del pipeline posterior se documentan por separado.

## Evidencia y próximos pasos

Audio, transcript, decisiones por bloque, comparación y candidatos se guardan
fuera de Git en `.local-env/podcast-benchmark/genwhy-700-20261007/`:

- `comparison.json`: métricas y desacuerdos.
- `candidate-low-v2/`, `candidate-high-v2/`: resultados raw.
- `candidate-reviewed-v2/`: copia con el inicio comercial corregido, `cuts.json`
  y `validation.json`.
- `clips/`: fragmentos de 24 segundos del mismo empalme, original/low/high/revisado.
- `verified-low-blocks/`, `verified-high-blocks/`: decisiones persistidas de los
  bloques leídos por los agentes.

El pipeline posterior añade revisión de límites con contexto anterior y
posterior, reintentos por bloque, invalidación por configuración, recuperación
ante reinicios y publicación atómica del archivo. Sigue pendiente validar por
escucha varios episodios etiquetados por una persona y conectar el hook a la
aplicación MoonStation. La API de ASR debe seguir anunciándose por capacidad
ejecutable; encontrar el framework no es suficiente.

No hay evidencia para habilitar autopublicación ahora. Las copias están listas
para escuchar y comparar, con el original preservado.
