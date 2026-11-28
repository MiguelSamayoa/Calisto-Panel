# Arquitectura — Calisto Panel

Referencia técnica. Contratos exactos, flujo de datos y registro de decisiones con su
justificación. Consultar solo cuando se necesite profundidad; las reglas de trabajo
diarias están en `CLAUDE.md`.

---

## 1. Componentes

| Componente | Proceso | Privilegio | Responsabilidad |
|---|---|---|---|
| LibreHardwareMonitor | externo | **administrador** | leer sensores; expone `127.0.0.1:8085/data.json` |
| Azure DevOps REST API | remoto | PAT con Build (Read) | último resultado por pipeline |
| `panel.exe` | propio | usuario normal | sondear, mantener estado, servir SSE, ejecutar acciones |
| Docker Desktop CLI | externo | usuario normal | `docker desktop start|stop|status`; `docker ps -a`, `docker stats --no-stream`, `docker start|stop <id>` |
| API de procesos de Windows | API | usuario normal | CPU y memoria por proceso (`internal/winutil`) |
| `vmrun.exe` | externo | usuario normal | `start nogui`, `stop soft`, `list` (solo lectura) |
| Sesión de medios de Windows (SMTC) | API WinRT | usuario normal, sesión interactiva | título, artista, posición y estado de lo que suena; saltar ±10 s |
| Windows SendInput | API | usuario normal, sesión interactiva | teclas de medios |
| Fully Kiosk Browser | Android | — | renderizar y disparar acciones |

`panel.exe` es el único componente que conoce credenciales. El teléfono recibe
exclusivamente estado derivado.

---

## 2. Flujo de datos

```
┌──────────────┐  cada 5 s   ┌─────────────────────────────┐
│ lhm.Client   │────────────►│ hub.Store                   │
└──────────────┘             │                             │
┌──────────────┐  cada 60 s  │  sensors: map[id]Item       │
│ CIProvider   │────────────►│  ci:      map[id]Item       │──► SSE por dominio
└──────────────┘             │  actions: map[exec]Item     │
┌──────────────┐  on demand  │                             │
│ actions.Run  │────────────►│  cada Item guarda updatedAt │
└──────────────┘             └─────────────────────────────┘
```

Reglas del store:

- Cada ítem guarda su `updatedAt`. El estado `ok`/`stale`/`dead` **se calcula al emitir**,
  no se almacena. Un poller puede reportar cada ítem de dos formas:
  - *fresco*: guarda el valor y refresca `updatedAt`;
  - *con falla* (`unavailable`, `auth` o `error`): el estado se emite de inmediato y los
    datos salen vacíos (`value` 0, `result`/`build`/`url` en `null`).

  Si el poller no reporta nada (p. ej. LHM no responde: solo se registra un `warn`), el
  último valor se conserva y el envejecimiento lo degrada solo a `stale` y luego `dead`.
  Un ítem que nunca recibió datos es `unavailable`.
- Los umbrales `stale`/`dead` de `config.yaml` rigen para `sensors`. Para `ci`, que se
  sondea a 60 o 150 s, se escalan con su intervalo: `stale = max(stale_ms, 2 × intervalo)` y
  `dead = max(dead_ms, 4 × intervalo)`. Sin esto un dato de CI recién sondeado estaría
  `stale` casi todo el tiempo. Para sensores (5 s) la fórmula da los mismos 10 s y 60 s.
- Al conectar, el hub envía `hello` y una foto del estado actual de `sensors` y `ci`
  (y `actions` si hay ejecuciones recientes), para que el teléfono no espere al siguiente
  sondeo. Un suscriptor que no consume y llena su búfer se descarta; su `EventSource`
  reconecta solo y recibe una foto nueva.
- Los dominios son independientes. Un fallo en LHM no toca los ítems de CI y al revés.
- El store no persiste. Al reiniciar `panel.exe` todo arranca en `unavailable` y se
  llena con el primer sondeo exitoso.
- Cada dominio emite su evento justo después de cada sondeo, no en un temporizador
  propio del transporte.

### Sondeo y resiliencia

| Fuente | Intervalo | Timeout | Ante error |
|---|---|---|---|
| LHM | 5 s | 2 s | log en nivel `warn`, reintento al siguiente tick; sin backoff (es local y barato) |
| Azure DevOps | 60 s o 150 s | 10 s | backoff exponencial hasta 5 min; se restablece al primer éxito |
| Medios (SMTC) | 2 s (+ re-lectura tras cada acción) | 3 s | `state:"error"` conservando los últimos datos; sin backoff |
| VM (`vmrun list`) | 5 s | 5 s | `state:"error"`; sin backoff (es local) |
| Procesos | 5 s | — | se conserva la última lista y el dominio pasa a `error`; sin backoff (es local) |
| Docker | 5 s | 8 s | `docker ps` fallido = motor apagado (`engine:false`, no es un error); sin backoff |
| Acción | — | 60 s | se marca `failed` con el mensaje de error |

`processes` y `docker` son dominios de **lista dinámica**: cada sondeo reemplaza la lista
completa y la salud (`state`, `ageMs`) es del dominio, no de cada ítem. Usan los umbrales
`stale_ms`/`dead_ms` de los sensores. El primer sondeo de `processes` solo toma la línea
base de CPU (un porcentaje necesita dos muestras) y no emite; hasta el segundo, el dominio
es `unavailable`. `docker` solo existe si `actions.docker.enabled` es `true`.

---

## 3. Contrato SSE

```
GET /api/stream
Accept: text/event-stream
```

Respuesta: `Content-Type: text/event-stream`, `Cache-Control: no-cache`,
`Connection: keep-alive`, `X-Accel-Buffering: no`.

Primera línea de la conexión: `retry: 3000`.
Cada 15 s sin tráfico: una línea de comentario `: keepalive`. El WebView de Android y
Fully Kiosk cortan conexiones ociosas; sin esto el panel se congela en silencio.

### Evento `hello` (una vez, al conectar)

```
event: hello
data: {"server":"calisto-panel/0.1.0","sensorsIntervalMs":5000,"ciIntervalMs":60000,"staleMs":10000,"deadMs":60000,"actions":["docker.start","docker.stop","vm.stop","vm.start","media.playpause","media.prev","media.next","media.seek_back","media.seek_fwd"],"dynamicActions":["container.start","container.stop","project.start","project.stop"],"processesIntervalMs":5000,"processesTopN":5,"confirmRequired":["docker.stop","vm.suspend","container.stop","project.stop"]}
```

El cliente construye los botones y las reglas de confirmación a partir de este evento.
No codifica la lista de acciones en el HTML. `dynamicActions` son familias de acciones
cuyo ID lleva un sufijo (ver §4); está vacío si Docker está deshabilitado. Una entrada de
`confirmRequired` cubre también sus IDs con sufijo: `container.stop` exige doble toque en
`container.stop.<id>`. Con Docker habilitado, el hub añade siempre `container.stop` y
`project.stop` a `confirmRequired` aunque `config.yaml` no los liste: el cliente no puede
conocer esos IDs de antemano y la confirmación no debe depender de acordarse de ponerlos.

### Evento `sensors`

```
event: sensors
data: {"ts":"2026-11-23T08:14:05-06:00","items":[
  {"id":"cpu.load","label":"CPU","value":23.4,"unit":"%","state":"ok"},
  {"id":"cpu.temp","label":"CPU temp","value":52.0,"unit":"°C","state":"ok"},
  {"id":"gpu.temp","label":"GPU temp","value":0,"unit":"°C","state":"unavailable"},
  {"id":"ram.used","label":"RAM","value":11.8,"unit":"GB","state":"stale","ageMs":14200}
]}
```

- `value` es `float64` crudo. Nunca una cadena, nunca con unidad incrustada.
- `unit` es la unidad reportada por la fuente, para que el cliente la pinte tal cual.
- `ageMs` aparece solo cuando `state` es `stale` o `dead`.
- Un ítem `unavailable` conserva su `id` y `label` para que el tile no desaparezca del
  layout. Un tile que se esfuma es peor que uno que dice que no sabe.

### Evento `processes`

```
event: processes
data: {"ts":"2026-11-23T08:14:05-06:00","state":"ok","memTotal":25553551360,"items":[
  {"id":"chrome.exe","label":"chrome.exe","cpu":2.5,"mem":2021441536,"count":22},
  {"id":"code.exe","label":"Code.exe","cpu":1.5,"mem":1794179072,"count":13}
]}
```

- Un ítem es una **app**: todos los procesos de un mismo ejecutable, sumados.
- `cpu` es el porcentaje de la capacidad total (todos los procesadores lógicos) usado
  desde el sondeo anterior. `mem` son bytes de memoria privada (`PrivateUsage`), que no
  cuenta dos veces las páginas compartidas entre los muchos procesos de un navegador.
  Ambos son crudos; el cliente formatea.
- Es app todo ejecutable cuya ruta **no** está bajo `%SystemRoot%`. `processes.exclude`
  oculta nombres concretos; `processes.include_unreadable` admite procesos cuya ruta
  Windows no deja leer sin elevación (`vmmem`/`vmmemWSL`, donde vive el consumo de Docker
  con WSL2). Un proceso ilegible que no esté en esa lista se descarta.
- El servidor envía la unión del top `top_n` por CPU y el top `top_n` por memoria,
  ordenada por CPU; el cliente arma las dos columnas.
- `memTotal` son los bytes de memoria física instalada, para que el cliente calcule la
  cuota de cada app. Los umbrales de presentación (piso de ruido del 1 % de CPU y 2 % de
  RAM, rojo desde el 10 %) y el promedio de las últimas 3 lecturas son del cliente: el
  servidor no filtra ni promedia.
- `state` es del dominio. Con `error` la lista es la última buena.

### Evento `docker`

```
event: docker
data: {"ts":"...","state":"ok","engine":true,"items":[
  {"id":"aaaaaaaaaaaa","project":"forms-dev","name":"forms-dev-web-1","image":"nginx:1.27","status":"Up 3 hours","state":"running","ports":"0.0.0.0:8080->80/tcp","cpu":1.25,"mem":11010048,"memLimit":8160437862.4,"pids":4},
  {"id":"cccccccccccc","project":"","name":"old-worker","image":"redis:7","status":"Exited (0) 5 days ago","state":"exited","ports":"","cpu":null,"mem":null,"memLimit":null,"pids":null}
]}
```

- `engine:false` significa que `docker ps` no pudo hablar con el motor (Docker Desktop
  apagado); `items` va vacío. Es un estado normal, no un fallo: `state` sigue siendo `ok`.
- `project` es el proyecto de Docker Compose (etiqueta `com.docker.compose.project`), o
  `""` si el contenedor es suelto. Un nombre que no tenga forma de proyecto válido
  (`[a-z0-9][a-z0-9_-]*`) se trata como suelto. El cliente agrupa por él y muestra solo el
  padre; los hijos se despliegan a demanda.
- `id` es el ID corto de 12 dígitos hexadecimales. Es lo que el cliente devuelve en
  `container.start.<id>` / `container.stop.<id>`. Una fila con otro formato se descarta.
- `status` es el texto de Docker, sin tocar. `state` es `running | exited | paused | ...`.
- `cpu`, `mem`, `memLimit` y `pids` son `null` en contenedores detenidos o si
  `docker stats` falló en esa ronda. `cpu` es un porcentaje; `mem` y `memLimit`, bytes.

### Evento `media`

```
event: media
data: {"ts":"...","state":"ok","active":true,"status":"playing","app":"Spotify.exe","sid":"3a1c9e5d0b72","title":"Song","artist":"Band","album":"","position":84.2,"duration":201.0,"volume":42,"cover":"3fa9c01b22de"}
```

- Es la sesión de medios actual de Windows (SMTC, la misma que usa el control de volumen):
  la app que Windows considera "en reproducción", sea Spotify, Chrome o el reproductor
  que sea. `active:false` si no hay ninguna.
- `status` es `playing | paused | stopped | other`. `position` y `duration` son segundos
  crudos; `duration` es 0 si la fuente no la informa (directos). La posición llega
  extrapolada al instante del sondeo; entre eventos el cliente la sigue avanzando él
  mismo mientras `status` sea `playing`.
- `cover` es el id corto (hash) de la portada de la pista, o `""` si no hay. Cambia cuando
  cambia la imagen. El cliente pide la imagen a `GET /api/media/cover?v=<cover>` (sin auth,
  como el resto de GET de solo lectura; 404 si no hay) y muestra un esqueleto mientras no
  haya imagen. El servidor la lee de SMTC una vez por pista (hasta 5 reintentos, porque la
  fuente suele publicarla después del título), la guarda solo en memoria (máx. 2 MiB) y no
  escribe nada a disco.
- `sessions` (D-35) lista **todas** las sesiones SMTC: `{id,app,status,title,artist,position,
  duration}`. `id` es el hash corto (12 hex) de la app de origen: estable entre sondeos, no un
  índice. `apps` lista las aplicaciones con sesión de audio: `{id,name,volume}` (`id` = hash
  del nombre del ejecutable; las sesiones de un mismo exe, p. ej. todas las pestañas de
  Chrome, se agrupan y se mueven juntas). Ambas son `[]` si no hay ninguna. Los campos
  de arriba siguen describiendo la sesión actual (la tarjeta grande).
- `sid` es el `id` de esa sesión actual dentro de `sessions` (`""` si no hay). El cliente lo
  usa para que los botones grandes actúen sobre la sesión que muestra la tarjeta y no sobre
  la tecla de medios global: con dos sesiones de Chrome abiertas, Chrome decide solo a cuál
  entrega la tecla.
- El sobre es un único objeto; las listas `sessions` y `apps` van dentro. Con un fallo de
  lectura, `state:"error"` y se conservan los últimos datos.
- Cadencia de 2 s. Tras cada acción de medios que funcionó se vuelve a leer a los ~0,25 s,
  ~0,7 s y ~1,5 s (Chrome tarda en publicar un play o una pausa), para que el icono de play/pausa y la tarjeta cambien al instante. Solo existe
  si `actions.media.enabled` es `true`.

### Evento `vm`

```
event: vm
data: {"ts":"...","state":"ok","name":"MI_VM","vm":"running","cpu":0.6,"mem":316252160,"running":1}
```

- `name` es el `.vmx` configurado sin extensión. `vm` es `running | suspended | off`:
  `running` si `vmrun list` lo nombra, `suspended` si no y existe su `.vmss`, `off` en
  otro caso. No confundir con `state`, que es la salud del dato.
- `cpu` (porcentaje de la CPU del host) y `mem` (bytes de memoria privada) salen del
  proceso `vmware-vmx.exe`; son 0 si la VM no corre. Con varias VMs encendidas los números
  suman todas, y `running` dice cuántas hay.
- Cadencia de 5 s; solo existe si `actions.vmware.enabled` es `true`.

### Evento `ci`

```
event: ci
data: {"ts":"2026-11-23T08:14:05-06:00","items":[
  {"id":"ci.42","label":"AvanzaRH · main","state":"ok","result":"succeeded","build":"20261123.3","finished":"2026-11-23T07:51:12Z","url":"https://dev.azure.com/..."},
  {"id":"ci.57","label":"SmartHealth · api","state":"ok","result":"running","build":"20261123.1","finished":null,"url":"https://..."},
  {"id":"ci.61","label":"Infra · deploy","state":"auth","result":null,"build":null,"finished":null,"url":null}
]}
```

Separación deliberada de dos ejes:

- `state` = salud del dato: `ok | stale | dead | unavailable | auth | error`.
- `result` = qué dice el dato: `succeeded | partiallySucceeded | failed | canceled | running | queued`.

Un pipeline rojo es `state:"ok", result:"failed"` — el dato es fresco y confiable, y dice
que falló. Un PAT caducado es `state:"auth", result:null` — no hay dato. Colapsar ambos
ejes en un campo haría indistinguible "falló el build" de "falló el panel", que es
exactamente el error que este diseño evita.

### Vigilancia de la conexión (cliente) y registro de conexiones (servidor)

`EventSource` reconecta solo tras un error, pero una conexión que **se queda en silencio**
sin error (la radio del teléfono se durmió, MIUI congeló el WebView) no lanza ninguno.
Por eso la página vigila el silencio: sin ningún evento en 20 s reconstruye el stream (como
mucho cada 10 s), y si pasan 45 s sin recuperarse recarga la página entera (como mucho una
vez por minuto). También reconstruye el stream al volver a primer plano y al volver la red.

El servidor registra cada conexión del stream: `stream opened` y `stream closed` con la
dirección remota y la duración. Cortes frecuentes y cortos desde la IP del teléfono
apuntan a que el teléfono suelta la conexión; ausencia total de `stream opened` tras una
caída, a que el teléfono ni siquiera intenta reconectar (proceso muerto o pantalla en
suspensión).

### Evento `actions`

```
event: actions
data: {"ts":"...","items":[
  {"id":"docker.stop","exec":"01JF3K8S2M","state":"running","message":null},
  {"id":"vm.start","exec":"01JF3K2P9A","state":"ok","message":null},
  {"id":"media.next","exec":"01JF3K0X4C","state":"failed","message":"exit status 1"}
]}
```

`state` de una acción: `running | ok | failed | timeout`. Los ítems se retienen 60 s tras
terminar y luego se descartan.

---

### Logs de un proyecto o de un contenedor

```
GET /api/logs/{proyecto}
GET /api/logs/container/{id}
Authorization: Bearer <api_token>
```

Devuelven `{"kind":"project|container","name":"...","containers":[{"id","name","state"}],"lines":[{"t","c","m"}]}`:
las últimas 400 líneas de **todos** los contenedores del proyecto (mezcladas por hora), o
las del contenedor indicado.
`t` son milisegundos Unix, `c` el ID del contenedor y `m` el texto (sin códigos de color
ANSI, recortado a 2000 caracteres). El cliente lo sondea cada 3 s mientras la vista de
detalle está abierta.

- **Exige el bearer**, a diferencia del resto de los `GET`: los logs suelen contener
  secretos (cadenas de conexión, tokens, datos de usuarios). `401` sin él. El token solo
  va en el encabezado, nunca en la URL.
- `{proyecto}` debe cumplir `[a-z0-9][a-z0-9_-]{0,62}` y ser un proyecto que el servidor
  listó en su último sondeo; `{id}` debe ser 12 dígitos hexadecimales y un contenedor que
  también listó. Si no, `404 not_found` y no se ejecuta nada.
- Se ejecuta `docker logs --tail 200 --timestamps <id>` con argv fijo, uno por contenedor
  (máximo 12), con los IDs que el hub mismo obtuvo. Las llamadas se serializan y el
  conjunto tiene un timeout de 6 s. Un contenedor que falla no hunde la respuesta.
- El texto de los logs lo controla quien escribe en el contenedor: el cliente lo pinta
  siempre como nodo de texto, nunca como HTML.

## 4. Contrato de acciones

```
POST /api/do/{actionId}
Authorization: Bearer <api_token>
```

Sin cuerpo. El `actionId` debe pertenecer a la lista blanca o a una de sus dos familias
con sufijo (ver "Acciones por contenedor" y "Acciones por proyecto" más abajo).

Orden de validación, fijo: **401 → 404 → 409**. La autenticación va primero para que un
cliente sin token no pueda sondear qué IDs existen.

**No hay límite de frecuencia global.** Antes había uno (1 acción cada 2 s y el `429`);
se quitó porque bloqueaba las acciones de medios, que deben poder repetirse al instante.
Lo que queda:

- Una ejecución concurrente por ID de acción: el mismo ID otra vez mientras corre da `409`.
- Las acciones `media.*` son instantáneas: no esperan, nunca dan `409` y un fallo es lo
  único que se reporta.
- Evitar el doble disparo de un par encender/apagar es de la **interfaz**: al pulsar
  "encender" ese botón se bloquea y "apagar" se desbloquea al instante, aunque la acción
  siga corriendo (levantar Docker tarda). Mientras no se pulsa nada, el bloqueo sigue lo
  que se observa (Docker encendido bloquea "encender"). Si la acción falla, se desbloquea;
  si termina bien pero lo observado no cambia en 8 s, también.
- `rate_limit_ms` de `config.yaml` está obsoleto: se acepta y se ignora, para que una
  configuración antigua siga cargando.

| Código | Cuerpo | Cuándo |
|---|---|---|
| `202` | `{"exec":"01JF3K8S2M","action":"docker.stop","state":"running"}` | aceptada; el resultado llega por el evento `actions` |
| `401` | `{"error":"unauthorized"}` | token ausente o incorrecto |
| `404` | `{"error":"unknown_action"}` | ID fuera de la lista blanca |
| `409` | `{"error":"already_running","exec":"01JF3K8S2M"}` | ya hay una ejecución de ese mismo ID |

Ejecución **asíncrona**: el `POST` responde de inmediato. `docker desktop stop` tarda
decenas de segundos y bloquear la petición dejaría el panel aparentemente colgado.

Detalles de la implementación (`internal/actions`):

- Solo se registran las acciones de los grupos con `enabled: true`; las demás responden
  `404 unknown_action`. La lista del evento `hello` sale del mismo registro.
- El ID `exec` son 10 caracteres Crockford base32 con los milisegundos de inicio (la mitad
  de tiempo de un ULID): ordenable, y único porque el ejecutor reparte milisegundos
  estrictamente crecientes aunque dos acciones empiecen en el mismo.
- Al cancelarse el contexto de `main`, las acciones en curso se matan y se reportan
  `failed`; `Executor.Wait()` espera a que todas reporten antes de salir.
- El mensaje de un fallo es el error del proceso (`running docker: exit status 1`),
  truncado a 200 caracteres. La salida del proceso se descarta.
- `actions.docker.status_running_match` **aún no se usa**: el contrato no define ningún
  evento ni tile que lea `docker desktop status`. Queda en la configuración para cuando
  se defina.

### Resolución a proceso

El `actionId` se resuelve contra `config.yaml` y produce un `argv` fijo:

| actionId | argv resultante |
|---|---|
| `docker.start` | `docker desktop start` |
| `docker.stop` | `docker desktop stop` |
| `vm.start` | `<vmrun_path> -T ws start <vmx> nogui` |
| `vm.stop` | `<vmrun_path> -T ws stop <vmx> soft` (apagado ordenado del invitado; requiere VMware Tools) |
| `media.playpause` | `SendInput` con `VK_MEDIA_PLAY_PAUSE` (`0xB3`) |
| `media.prev` | `SendInput` con `VK_MEDIA_PREV_TRACK` (`0xB1`, configurable con `prev_vk`) |
| `media.next` | `SendInput` con `VK_MEDIA_NEXT_TRACK` (`0xB0`) |
| `media.seek_back` | SMTC `TryChangePlaybackPositionAsync(posición − 10 s)`, acotado al inicio de la pista |
| `media.seek_fwd` | SMTC `TryChangePlaybackPositionAsync(posición + 10 s)`, acotado al final de la pista |

No existe una tecla de medios global para saltar 10 s, por eso el avance y el retroceso
van por SMTC. Fallan con un mensaje claro si no hay sesión o la fuente no deja buscar.

Ningún valor del cliente entra en el `argv`. El cliente solo elige una fila de esa tabla
(salvo las dos familias de contenedor, que siguen).

### Acciones por contenedor

| actionId | argv resultante |
|---|---|
| `container.start.<id>` | `docker start <id>` |
| `container.stop.<id>` | `docker stop <id>` |

Es la única excepción a "lista cerrada de IDs", y está acotada así:

1. El verbo es uno de dos literales; cualquier otro (`restart`, `rm`, `exec`…) es `404`.
2. `<id>` debe ser exactamente 12 dígitos hexadecimales en minúscula. Por construcción no
   puede empezar por `-`, contener espacios ni parecer una opción.
3. `<id>` debe estar en la lista que **el servidor mismo** obtuvo en su último sondeo
   exitoso de `docker ps` (`hub.HasContainer`). El cliente nunca nombra un objetivo que el
   servidor no haya observado: un ID bien formado pero desconocido, o cualquiera mientras
   el motor está apagado, responde `404 unknown_action`.
4. El `argv` se arma en `internal/actions`; el ID validado es su único elemento variable.
5. Solo existe con `actions.docker.enabled: true`.

### Volumen (`media.volume.<n>`)

`media.volume.<n>` fija el volumen maestro del dispositivo de reproducción predeterminado
(Core Audio, `IAudioEndpointVolume.SetMasterVolumeLevelScalar`, llamado a mano como SMTC,
sin dependencias). Es la segunda excepción a la lista cerrada (D-34) y se acota así:

1. `<n>` es la forma canónica de un entero de 0 a 100: de uno a tres dígitos, sin signo, sin
   espacios y sin ceros a la izquierda (`007`, `101`, `-1`, `5x` → `404 unknown_action`).
2. No hay `argv` ni proceso: el entero validado solo llega a la llamada de Core Audio.
3. Es una acción de medios: instantánea, nunca `409`, sin entrada en `actions` si funciona.
4. Solo existe con `actions.media.enabled: true`. Exige bearer como todo `POST`.

El evento `media` trae `volume` (porcentaje 0-100, `-1` si no se pudo leer) en cada sondeo.

### Sesiones y volumen por app (`media.session.*`, `media.app.volume.*`) — D-35

Tercera excepción a la lista cerrada, con la misma disciplina que D-31:

- `media.session.<verbo>.<sid>`: `verbo` ∈ `playpause|prev|next|seek_back|seek_fwd`. Usa los
  controles de esa sesión SMTC (`TryTogglePlayPause`, `TrySkipNext/Previous`,
  `TryChangePlaybackPosition`), no teclas de medios globales.
- `media.app.volume.<aid>.<n>`: fija `ISimpleAudioVolume` de todas las sesiones de audio de
  esa app (Core Audio, `IAudioSessionManager2`, como EarTrumpet; sin elevación).
- `<sid>` y `<aid>` son 12 hex en minúscula **y** deben figurar en la lista `sessions`/`apps`
  que el propio servidor publicó en su último sondeo; si no, `404 unknown_action`. `<n>` sigue
  la forma canónica 0-100 de D-34. Ninguno llega a un `argv`.
- Acciones de medios: instantáneas, nunca `409`, solo con `actions.media.enabled`, bearer
  como todo `POST`. No se usa el PID (se recicla); la identidad es el hash del nombre.
- Nombre de la app de audio: las sesiones de audio se agrupan por ejecutable. Si el proceso es
  un anfitrión auxiliar (`msedgewebview2.exe`, que WhatsApp y otras apps embeben), se sube al
  proceso padre para que el volumen y el nombre sean los de la app real. La etiqueta es la
  `FileDescription` del ejecutable (`Google Chrome`) o, si no se puede leer (apps de la
  Tienda), el nombre del archivo sin extensión (`WhatsApp`).

#### Interfaz del panel Multimedia

Columna derecha con **una tarjeta por app de audio**: nombre, una sola barra de volumen
(Windows tiene un volumen por app) y debajo todas las sesiones de esa app, cada una con su
título y sus botones ⏮ ⏯ ⏭. El cliente une sesión y app por nombre: SMTC dice `Chrome`,
`Chrome._crx_<hash>` (una PWA) o `5319275A.WhatsAppDesktop_…!App`; Core Audio dice
`Google Chrome` o `WhatsApp`; coinciden si algún fragmento de 4 o más letras del id de la
sesión contiene el nombre de la app, o al revés. Una sesión sin app de audio, o una app sin
sesión, tiene su tarjeta con lo que tiene. Lo que suena va primero.

Límites conocidos:

- Solo hay botones donde la app publica una sesión SMTC. Steam, los juegos y muchas apps no
  lo hacen: tienen barra de volumen y nada más. Windows no ofrece play/pausa para audio ajeno.
- Una sesión puede aceptar los comandos y descartarlos sin error (observado con un video de
  YouTube en la sesión `Chrome`: pausa, play, alternar y ±10 s devolvían éxito y no
  cambiaban nada; la PWA de YouTube Music sí obedecía). No es algo que el servidor pueda
  detectar ni corregir.

### Acciones por proyecto

| actionId | argv resultante |
|---|---|
| `project.start.<nombre>` | `docker start <id1> <id2> …` |
| `project.stop.<nombre>` | `docker stop <id1> <id2> …` |

`<nombre>` solo se usa como **clave de búsqueda**: debe cumplir `[a-z0-9][a-z0-9_-]{0,62}`
y ser un proyecto que el servidor listó en su último sondeo (`hub.ProjectContainers`).
Los `<id>` del `argv` son los que **el servidor mismo** obtuvo y validó (12 hex) para ese
proyecto; el cliente no aporta ninguno. Con el motor apagado, o con un proyecto
desconocido, responde `404 unknown_action`. `docker stop` detiene los contenedores uno
tras otro (hasta 10 s de gracia cada uno): un proyecto con muchos puede acercarse al
timeout de 60 s, en cuyo caso se reporta `timeout` aunque Docker siga deteniéndolos.

Heredan el resto del contrato: bearer obligatorio, una ejecución concurrente por ID completo (por eso el `409` es por contenedor), timeout
de 60 s y entrada en el evento `actions` con el ID completo. `container.stop` y
`project.stop` figuran siempre en `confirmRequired`, así que el cliente exige doble toque.

Las teclas de medios usan `SendInput` de `user32.dll`. `golang.org/x/sys/windows` **no**
trae un envoltorio de `SendInput`: se declara con
`windows.NewLazySystemDLL("user32.dll").NewProc("SendInput")` en `internal/winutil/` y se
llama con una estructura `INPUT` propia (`type = INPUT_KEYBOARD`, `wVk` = código virtual,
una pulsación `keydown` y otra con `KEYEVENTF_KEYUP`). No se añade ninguna dependencia
nueva. Requieren que el proceso viva en la sesión interactiva del usuario, de donde se
deriva la decisión D-09.

---

## 5. Contrato de LibreHardwareMonitor

```
GET http://127.0.0.1:8085/data.json
```

El cuerpo es un árbol homogéneo. Cada nodo:

```json
{
  "id": 19,
  "Text": "CPU Core",
  "Min": "41,0 °C",
  "Value": "41,0 °C",
  "Max": "77,5 °C",
  "SensorId": "/lpc/nct6795d/temperature/0",
  "Type": "Temperature",
  "ImageURL": "images/transparent.png",
  "Children": []
}
```

- La raíz tiene `Text: "Sensor"` y `Value: "Value"` — los nodos intermedios llevan
  cadenas de cabecera, no datos. Solo importan las **hojas con `SensorId`**.
- `Min`, `Value` y `Max` son **cadenas con unidad y separador decimal del locale**.
- Los nodos de agrupación (equipo, placa, categoría) no tienen `SensorId`.

### Algoritmo

1. Recorrer `Children` recursivamente.
2. Indexar por `SensorId` las hojas que lo tengan.
3. Para cada `sensors.items[]` de la configuración, buscar su `sensor_id`:
   - presente → parsear `Value`, estado `ok`
   - ausente → estado `unavailable`, registrar el error **una vez** (no en cada tick)
4. Parsear `Value`:
   - separar el número de la unidad; el separador puede ser un espacio normal o un
     espacio duro (`U+00A0`)
   - **el último separador presente (`.` o `,`) es el decimal**; los anteriores son de
     miles y se eliminan
   - `"-"`, cadena vacía o no numérica → `unavailable`
5. Las versiones recientes de LHM añaden `RawValue` (también una cadena con unidad). Si
   existe se parsea en lugar de `Value`; si no, se usa `Value`. En ambos casos `unit` sale
   de la cadena parseada. `RawValue` viene en la unidad base: en el rendimiento de red
   `Value` dice `"1.3 KB/s"` y `RawValue` `"1290.1 B/s"`, así que el cliente recibe `B/s`
   y es él quien escala. El fixture real usa punto decimal; los casos con coma se prueban
   con cadenas sintéticas.

Casos de prueba obligatorios: `"41,0 °C"` → `41.0`; `"41.0 °C"` → `41.0`;
`"4.593,0 MHz"` → `4593.0`; `"1,000 V"` → `1.0`; `"11,8 GB"` → `11.8`; `"-"` →
`unavailable`.

### Validación al arrancar

Un `sensor_id` inexistente degrada su tile y nada más. `panel.exe` **no** aborta: el
usuario actualiza LHM o cambia de hardware sin que el panel entero muera, y el tile
degradado le dice exactamente qué re-mapear.

---

## 6. Contrato de Azure DevOps

Una sola petición por ciclo para todos los pipelines:

```
GET https://dev.azure.com/{organization}/{project}/_apis/build/builds
      ?definitions=42,57,61
      &maxBuildsPerDefinition=1
      &queryOrder=finishTimeDescending
      &api-version=7.1
Authorization: Basic base64(":" + PAT)
```

`maxBuildsPerDefinition=1` devuelve el último build de cada definición en una sola
llamada. Con 60 s de intervalo son 60 peticiones/hora en total, independientemente del
número de pipelines; con 150 s, 24. Una llamada por pipeline cada 5 s sería del orden de
2.880 peticiones/hora por pipeline y provocaría throttling.

El PAT va como Basic con usuario vacío: `Authorization: Basic ` +
base64(`":" + PAT`). Scope `Build (Read)`.

**Un PAT caducado o inválido no siempre responde `401`.** Azure DevOps suele contestar
`203 Non-Authoritative Information` con la página HTML de inicio de sesión, o un `302`
hacia el login. Un cliente que solo mire 401/403 interpretaría eso como un cuerpo
inválido (`error`) o, peor, como éxito. Reglas del cliente HTTP:

- No seguir redirecciones (`CheckRedirect` devuelve `http.ErrUseLastResponse`).
- Tratar como `auth` cualquier `401`, `403`, `203` o `302`, y cualquier `200` cuyo
  `Content-Type` no sea `application/json`.
- Solo un `200` con `application/json` se decodifica.

Tras el decodificado, toda definición configurada que **no** aparezca en `value[]` se
marca `unavailable` (la API la omite, no devuelve un ítem vacío).

### Mapeo a `state` y `result`

| Respuesta | `state` | `result` |
|---|---|---|
| `status: completed`, `result: succeeded` | `ok` | `succeeded` |
| `status: completed`, `result: partiallySucceeded` | `ok` | `partiallySucceeded` |
| `status: completed`, `result: failed` | `ok` | `failed` |
| `status: completed`, `result: canceled` | `ok` | `canceled` |
| `status: inProgress` o `cancelling` | `ok` | `running` |
| `status: notStarted` o `postponed` | `ok` | `queued` |
| definición ausente en la respuesta (sin builds) | `unavailable` | `null` |
| HTTP 401, 403, 203, 302, o `200` no JSON | `auth` | `null` |
| HTTP 429 o 5xx, o error de red | `error` | `null` |

Campos consumidos: `buildNumber`, `status`, `result`, `finishTime`, `definition.id`,
`definition.name`, `_links.web.href`. Todo lo demás se descarta.

### GitHub Actions

No implementado en la v1. La interfaz existe para que quepa después:

```go
type CIProvider interface {
    Name() string
    Poll(ctx context.Context) ([]Item, error)
}
```

`azuredevops.Provider` implementa `ci.CIProvider`. Un futuro `githubactions.Provider` se registra en
`main` sin tocar el hub ni el cliente.

---

## 7. Configuración

Dos archivos. `config.yaml` es versionable como ejemplo; `secrets.yaml` nunca entra al
repositorio.

```
C:\ProgramData\CalistoPanel\config.yaml
C:\ProgramData\CalistoPanel\secrets.yaml
C:\ProgramData\CalistoPanel\logs\panel.log
```

Esquema completo en `config.example.yaml`. Validación al arrancar: puerto en rango,
intervalos positivos, `stale_ms < dead_ms`, `vmrun_path` existe, rutas `.vmx` existen,
lista de acciones no vacía, token no vacío. Si `ci.azure_devops.pipelines` está vacío el
CI se desactiva: no se lanza el sondeo, el evento `ci` emite `items: []` y
`azure_devops_pat` no es obligatorio; con al menos un pipeline el PAT sí lo es. Un fallo de validación **sí** aborta el
arranque: es un error del operador, no una degradación en tiempo de ejecución.

Las secciones `processes`, `containers` y `vm`, y los campos `actions.media.prev_vk` y
`actions.media.interval_ms`, son opcionales: sin ellas rigen los valores por
defecto de `config.example.yaml` (5 s, `top_n: 5`, `include_unreadable: [vmmem, vmmemWSL]`,
timeout de Docker 8 s). Así una configuración anterior sigue cargando.

Los umbrales `stale_ms`/`dead_ms` y los intervalos del evento `hello` se leen de la
configuración; el código no define constantes propias para ellos.

Logging: `log/slog` en JSON hacia `lumberjack` con 5 archivos de 5 MB. Sin retención por
días: el panel no es una fuente de auditoría, y 25 MB cubren de sobra el tiempo útil de
un diagnóstico.

Toda ejecución de acción se registra con marca de tiempo, `actionId`, `exec`, resultado y
duración. Cuando un botón apague una VM que no debía, el log es lo que lo explica.

---

## 8. Modelo de amenaza

**En alcance:** otros dispositivos y personas de la red doméstica. Acceso físico al
teléfono encendido en el escritorio.

**Fuera de alcance:** atacante con sesión en el PC; atacante en la red del cliente a
través de la VPN de trabajo (se mitiga por construcción: la regla de firewall solo
aplica al perfil Privado); atacante en Internet (no hay exposición externa).

Consecuencias aceptadas:

1. Cualquiera en la LAN lee métricas y nombres de pipelines por `GET` sin autenticación.
   Aceptado: red familiar, y la alternativa es autenticar también la lectura, lo que
   obliga a introducir el token antes de poder mirar la pantalla.
2. Quien tenga el teléfono en la mano tiene el token (`localStorage`). Por eso ninguna
   acción es irreversible y las dos destructivas exigen doble toque.
3. El tráfico va en claro por la LAN. Sin TLS en la v1: un certificado autofirmado
   produce advertencias permanentes en el WebView, y el beneficio frente a este modelo de
   amenaza es nulo.
4. El teléfono es un dispositivo sin parches (Android 11 / MIUI 12.5.8) permanentemente
   en la LAN. Se mitiga dedicándolo: sin cuentas, sin navegación.
5. Cualquiera en la LAN lee por `GET` los nombres de contenedores, imágenes y puertos, y
   los nombres de las apps que consumen más. Misma lógica que el punto 1.
6. Los logs de contenedores pueden contener secretos. Por eso `GET /api/logs/{proyecto}` es
   la única lectura que exige el bearer (§3). Quien tenga el token del teléfono los lee;
   quien solo está en la LAN no. La tarjeta de "reproduciendo" sí es pública como el resto
   de los `GET`: muestra el título de lo que suena en el PC, lo que cualquiera en la LAN ya
   podría oír.
7. Con el token se puede encender o apagar cualquier contenedor que Docker liste, no solo
   los "del panel". Es reversible (`docker start`), pero puede cortar un servicio que
   alguien use. Por eso `container.stop` exige doble toque. Nombres de contenedor, imagen
   y puertos son texto controlado por quien crea el contenedor: el cliente los pinta
   siempre con `textContent`, nunca como HTML.
8. Endurecimiento del servidor HTTP: `MaxHeaderBytes` de 16 KiB, `ReadHeaderTimeout` de 5 s,
   `IdleTimeout` de 2 min y como máximo 16 streams SSE simultáneos (el 17.º recibe `503`).
   No hay `ReadTimeout` porque cancelaría el contexto del stream. El `action` de una
   petición sin token se recorta a 64 caracteres antes de registrarse. La salida de
   `docker logs` se limita a 1 MiB por contenedor (se conserva el final), y el proyecto
   Compose se pide a Docker por su etiqueta, no se extrae de la lista `Labels`.

---

## 9. Registro de decisiones

| ID | Decisión | Alternativas descartadas | Justificación |
|---|---|---|---|
| D-01 | Servicio propio en vez de herramienta existente | Touch Portal; Home Assistant + HASS.Agent; Grafana + Prometheus | Ninguna cubre CI + telemetría + botones a la vez. Touch Portal es un deck, no un dashboard, y su versión gratuita limita a 8 botones en una rejilla 4×2 con dos páginas. Home Assistant obliga a mantener una plataforma entera y su agente de Windows depende de forks de la comunidad tras quedar sin mantenimiento el original. Grafana no tiene botones reales. Además, el requisito explícito era obtener una pieza reutilizable |
| D-02 | Telemetría delegada a LibreHardwareMonitor | leer sensores desde Go | Temperaturas y GPU exigen acceso a nivel kernel y un driver en ring0. Es la parte más caprichosa del proyecto y ya está resuelta, gratis y mantenida. Sin este atajo el proyecto no cabía en el presupuesto de 10 h |
| D-03 | Go en vez de Java | Java/Spring; C#/.NET | Binario único estático, sin runtime residente en el PC de trabajo del usuario, arranque inmediato y bajo consumo —requisito explícito—; además está en su plan de aprendizaje |
| D-04 | Un `index.html` plano con `embed.FS` | Angular; React; cualquier paso de build | El usuario tiene poca experiencia en frontend. Cero `npm`, cero configuración de build, cero superficie donde atascarse. El binario sigue siendo uno |
| D-05 | SSE en vez de WebSockets o sondeo | WebSocket; `fetch` en bucle | El flujo es unidireccional servidor→cliente. SSE son unas líneas de `fmt.Fprintf` del lado Go y seis del lado cliente, y reconecta solo |
| D-06 | Eventos por dominio con cadencias separadas | snapshot único a 5 s | Un snapshot unificado arrastraría el sondeo de CI a 5 s y provocaría throttling en Azure DevOps. Ver §6 |
| D-07 | Valores crudos; el cliente formatea | el servidor entrega cadenas listas | Un cliente tonto es más fácil de depurar que un servidor que decide tipografía. También deja la puerta abierta a otro consumidor del mismo stream |
| D-08 | `state` y `result` como ejes separados en CI | un solo campo de estado | Distingue "el build falló" de "el panel falló". Colapsarlos convierte un PAT caducado en un falso rojo de pipeline. Ver §3 |
| D-09 | Tarea programada al iniciar sesión, **sin elevación** | servicio de Windows; tarea con privilegios máximos | En la sesión 0 las teclas de medios no llegan a ninguna aplicación. Y el proceso expuesto a la red no necesita privilegios de administrador: no los pide para `localhost`, `docker desktop`, `vmrun` ni `SendInput` |
| D-10 | `0.0.0.0:7171` + regla de firewall en perfil Privado restringida a la subred | bind a la IP de LAN concreta | Con bind a IP fija el servicio no arranca si la interfaz aún no tiene dirección al iniciar sesión. El confinamiento lo hace el firewall |
| D-11 | Perfil `Public` nunca habilitado en la regla | habilitar ambos perfiles | Al conectar una VPN de trabajo, Windows registra una interfaz que puede clasificarse como pública; con `Public` habilitado el panel quedaría expuesto en la red del cliente. La Ethernet sigue siendo Privada aunque la VPN sea Pública, así que la regla no abre la interfaz de la VPN. El panel solo deja de responder si la VPN corta o redirige la LAN local (colisión de subred, cortafuegos de WireGuard con `0.0.0.0/0`); ver README §4.1.1 |
| D-12 | Reserva DHCP en el router | IP estática en Windows; mDNS | La URL del teléfono debe ser estable y el teléfono no puede resolver un nombre que Windows no publica. La reserva lo resuelve en el punto correcto de la red |
| D-13 | `GET` sin auth, `POST` con bearer token | autenticar todo; sin auth | Requisito explícito: la familia puede ver, no ejecutar. Autenticar la lectura obligaría a introducir el token antes de poder mirar la pantalla |
| D-14 | Token introducido a mano una vez → `localStorage` | token en la URL de arranque de Fully Kiosk; QR | Un token en query string termina en historial, logs y encabezados `Referer` |
| D-15 | Sin TLS en la v1 | certificado autofirmado; mTLS | Un autofirmado produce advertencias permanentes en el WebView y no aporta nada frente al modelo de amenaza de §8 |
| D-16 | Lista blanca cerrada de 6 acciones, argv fijo (ampliada por D-31 con dos familias de contenedor) | endpoint genérico de comandos | Un endpoint que ejecuta lo que le manden es exactamente el agujero que este diseño evita, incluso con token |
| D-17 | `vmrun stop ... soft` (antes `suspend soft`), nunca `stop hard` | apagado forzado; suspender | Un toque accidental en una pantalla siempre encendida al borde del escritorio no debe destruir trabajo sin guardar |
| D-18 | Doble confirmación en `docker.stop` y `vm.stop`, en el cliente | confirmación en el protocolo; sin confirmación | Es una preocupación de interfaz. Un parámetro `confirm` en la API da falsa seguridad: cualquiera con el token lo envía igual |
| D-19 | Ejecución asíncrona con `exec` id | `POST` síncrono | `docker desktop stop` tarda decenas de segundos; bloquear dejaría el panel aparentemente colgado |
| D-20 | `sensor_id` explícitos + validación con degradación por tile | heurística por nombre de sensor | La heurística falla en silencio con valores equivocados. Los IDs explícitos fallan de forma visible y señalan qué re-mapear |
| D-21 | Umbrales `stale` 10 s / `dead` 60 s, con antigüedad visible | ocultar datos viejos; mostrarlos sin marca | Un dashboard que muestra datos viejos como si fueran actuales es peor que uno apagado |
| D-22 | Página de respaldo local en el teléfono cuando el PC está apagado | pantalla de error del navegador | Sin SIM, si el Wi-Fi o el PC caen el panel queda ciego; la página de error del navegador no comunica nada útil |
| D-23 | Host único, sin prefijos multi-host | `/api/hosts/{id}/...` desde el inicio | El día que haya varios PCs el problema real es descubrimiento y agregación, no la forma de la ruta. Un prefijo vacío hoy no lo ahorra |
| D-24 | Sin históricos ni base de datos **en el PC** | SQLite; Prometheus | El panel responde "¿cómo está ahora?". Las series temporales son otro producto. Excepción acotada en D-33 |
| D-25 | GitHub Actions detrás de `CIProvider`, sin implementar | ambos proveedores en la v1 | Presupuesto de 10 h. La interfaz deja el hueco sin costo |
| D-26 | Metadatos de música, estado de reproducción y ±10 s vía SMTC con llamadas WinRT directas desde Go (reemplaza el aplazamiento a v2) | ayudante en .NET; PowerShell; dependencia `winrt-go`; teclas de medios solamente | Lo piden el icono dinámico de play/pausa, la tarjeta de "reproduciendo" y el salto de 10 s, y los tres necesitan SMTC. Se resolvió sin cgo ni módulos nuevos: `internal/winutil/media_session_windows.go` activa `GlobalSystemMediaTransportControlsSessionManager` con `RoGetActivationFactory` y llama a su vtable a mano. Los IIDs y posiciones de vtable salen de `Windows.Media.Control.winmd` y se comprobaron contra la máquina real. Un ayudante externo habría roto la regla de argv fijo o añadido un proceso residente |
| D-27 | `docker desktop status` con cadena de coincidencia configurable | parseo fijo de la salida | La cadena varía entre versiones de Docker Desktop. Un valor en configuración se corrige sin recompilar |
| D-28 | Secretos en `secrets.yaml` aparte, con ACL restringida | todo en `config.yaml`; variables de entorno | Permite versionar `config.example.yaml` completo sin riesgo, y acotar permisos al único archivo que los necesita |
| D-29 | Tres documentos en vez de cinco | README + GUIA_USUARIO + CLAUDE + ARCHITECTURE + DECISIONS | Para ~1.500 líneas de Go, cinco documentos se desincronizan antes de terminar la v1. El registro de decisiones vive aquí como §9, no como archivo huérfano |
| D-30 | Paneles `processes` y `docker` como dominios SSE propios, de lista dinámica y con salud por dominio | ampliar `sensors`; un snapshot único | Cambian de tamaño en cada sondeo, así que no caben en los ítems fijos de `sensors`; y `docker stats` tarda 1-2 s, no debe frenar los sensores. CPU y RAM por proceso salen de la API de Windows con `golang.org/x/sys/windows` (ya permitida): LHM no los da. Se agrupa por ejecutable porque un navegador son decenas de procesos |
| D-31 | Acciones por contenedor (`container.*.<id>`) y por proyecto de Compose (`project.*.<nombre>`), validadas contra la lista que el servidor observó | lista cerrada sin contenedores; nombres en el argv; endpoint con el ID libre | El usuario quiere encender y apagar contenedores individuales desde el panel. Esto exige que el cliente envíe un identificador, lo que choca con la regla 3 de `CLAUDE.md`. Se resuelve sin abrir un canal de comandos: verbo de dos literales, ID de exactamente 12 hex (no puede ser una opción), y solo contenedores que el propio servidor listó. Las acciones por proyecto (Docker Desktop muestra los proyectos de Compose como un padre) no ponen el nombre en el `argv`: lo usan para buscar los IDs que el servidor ya conoce. Ver §4 |
| D-32 | Sin límite de frecuencia global; bloqueo de pares encender/apagar en la interfaz; acciones de medios instantáneas | mantener 1 acción cada 2 s; límite solo para las no-medios | El límite de 2 s impedía repetir ±10 s o siguiente, y no resolvía lo que de verdad molestaba (pulsar dos veces "encender"). El `409` por ID ya impide dos ejecuciones simultáneas de lo mismo, y la interfaz bloquea el botón pulsado y libera el contrario al instante. Riesgo aceptado: se puede pulsar "apagar" mientras "encender" aún corre (Docker tarda); lo decide el usuario, no el servidor |
| D-33 | Gráficas de 1 y 5 min con el historial **en el cliente** (memoria de la página + copia en `localStorage` cada 15 s) | historial en el servidor; base de datos; sin gráficas | El usuario pidió gráficas de CPU, RAM, temperaturas y red con ventana de 1 o 5 min. Un buffer de 6 min son ~70 puntos por sensor: cabe en el teléfono, y guardarlo allí respeta D-24 (el PC sigue sin almacenar nada) y la regla de no unir dominios. Se pierde si se borran los datos del navegador del teléfono, y una caída de red deja un hueco (la línea se corta, no se interpola). RAM se grafica como % de la memoria instalada (`memTotal` del evento `processes`) para compartir el eje 0-100 con la CPU. Rangos, colores y niveles de peligro son de presentación y viven en `web/index.html` |
| D-34 | Volumen con barra arrastrable: `media.volume.<0-100>` + lectura en el evento `media` | solo teclas de volumen (±2 %); endpoint con valor libre | El usuario quiere una barra. Fijar un nivel exacto exige que el cliente envíe un número, lo que choca con la regla 3. Se acota como D-31: el entero tiene forma canónica 0-100, no entra a ningún `argv` y solo alimenta `SetMasterVolumeLevelScalar`. El nivel real se publica en el evento `media` para que la barra siga a los cambios hechos en el PC |
| D-35 | Varias sesiones SMTC y volumen por app: `media.session.<verbo>.<sid>` y `media.app.volume.<aid>.<n>`, ids validados contra la última lista publicada | solo la sesión actual; PID como id; endpoint con nombre de proceso libre | El usuario quiere ver todo lo que suena y mezclar por app (como EarTrumpet). Ids = hash estable del nombre de app (no índice ni PID, que cambian o se reciclan); el servidor solo acepta ids que él mismo listó, igual que D-31. Sin argv ni procesos |

---

## 10. Trabajo diferido

| Tema | Condición para abordarlo |
|---|---|
| GitHub Actions | existe un pipeline real en GitHub que el usuario necesite vigilar |
| Acceso fuera de la LAN | Tailscale o WireGuard, nunca port forwarding; antes hay que comprobar que no choque con el cliente VPN de trabajo por tabla de rutas y DNS |
| TLS | solo si el modelo de amenaza de §8 cambia |
| Multi-host | cuando exista un segundo PC que lo justifique; implica descubrimiento y agregación, no solo cambiar rutas |
| Históricos de más de 5 minutos | producto distinto; la respuesta probable es Grafana + Prometheus, no este binario |
