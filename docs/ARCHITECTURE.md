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
| Docker Desktop CLI | externo | usuario normal | `docker desktop start|stop|status` |
| `vmrun.exe` | externo | usuario normal | `start nogui`, `suspend soft` |
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
| Acción | — | 60 s | se marca `failed` con el mensaje de error |

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
data: {"server":"calisto-panel/0.1.0","sensorsIntervalMs":5000,"ciIntervalMs":60000,"staleMs":10000,"deadMs":60000,"actions":["docker.start","docker.stop","vm.suspend","vm.start","media.playpause","media.next"],"confirmRequired":["docker.stop","vm.suspend"]}
```

El cliente construye los botones y las reglas de confirmación a partir de este evento.
No codifica la lista de acciones en el HTML.

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

## 4. Contrato de acciones

```
POST /api/do/{actionId}
Authorization: Bearer <api_token>
```

Sin cuerpo. El `actionId` debe pertenecer a la lista blanca.

Orden de validación, fijo: **401 → 404 → 409 → 429**. La autenticación va primero para que
un cliente sin token no pueda sondear qué IDs existen. El `409` va antes que el `429` para
que un doble toque sobre una acción en curso informe `already_running` y no consuma la
ventana de frecuencia.

| Código | Cuerpo | Cuándo |
|---|---|---|
| `202` | `{"exec":"01JF3K8S2M","action":"docker.stop","state":"running"}` | aceptada; el resultado llega por el evento `actions` |
| `401` | `{"error":"unauthorized"}` | token ausente o incorrecto |
| `404` | `{"error":"unknown_action"}` | ID fuera de la lista blanca |
| `409` | `{"error":"already_running","exec":"01JF3K8S2M"}` | ya hay una ejecución de ese mismo ID |
| `429` | `{"error":"rate_limited","retryAfterMs":2000}` | más de una acción cada 2 s en total |

Ejecución **asíncrona**: el `POST` responde de inmediato. `docker desktop stop` tarda
decenas de segundos y bloquear la petición dejaría el panel aparentemente colgado.

Detalles de la implementación (`internal/actions`):

- Solo se registran las acciones de los grupos con `enabled: true`; las demás responden
  `404 unknown_action`. La lista del evento `hello` sale del mismo registro.
- El ID `exec` son 10 caracteres Crockford base32 con los milisegundos de inicio (la mitad
  de tiempo de un ULID): ordenable, y único porque los inicios están limitados a uno cada 2 s.
- Una petición rechazada (404, 409 o 429) no consume la ventana de frecuencia.
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
| `vm.suspend` | `<vmrun_path> -T ws suspend <vmx> soft` |
| `media.playpause` | `SendInput` con `VK_MEDIA_PLAY_PAUSE` (`0xB3`) |
| `media.next` | `SendInput` con `VK_MEDIA_NEXT_TRACK` (`0xB0`) |

Ningún valor del cliente entra en el `argv`. El cliente solo elige una fila de esa tabla.

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

Los umbrales `stale_ms`/`dead_ms` y los intervalos del evento `hello` se leen de la
configuración; el código no define constantes propias para ellos.

Logging: `log/slog` en JSON hacia `lumberjack` con 5 archivos de 5 MB. Sin retención por
días: el panel no es una fuente de auditoría, y 25 MB cubren de sobra el tiempo útil de
un diagnóstico.

Toda ejecución de acción se registra con marca de tiempo, `actionId`, `exec`, resultado y
duración. Cuando un botón suspenda una VM que no debía, el log es lo que lo explica.

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
| D-16 | Lista blanca cerrada de 6 acciones, argv fijo | endpoint genérico de comandos | Un endpoint que ejecuta lo que le manden es exactamente el agujero que este diseño evita, incluso con token |
| D-17 | `vmrun suspend ... soft`, nunca `stop hard` | apagado forzado | Un toque accidental en una pantalla siempre encendida al borde del escritorio no debe destruir trabajo sin guardar |
| D-18 | Doble confirmación en `docker.stop` y `vm.suspend`, en el cliente | confirmación en el protocolo; sin confirmación | Es una preocupación de interfaz. Un parámetro `confirm` en la API da falsa seguridad: cualquiera con el token lo envía igual |
| D-19 | Ejecución asíncrona con `exec` id | `POST` síncrono | `docker desktop stop` tarda decenas de segundos; bloquear dejaría el panel aparentemente colgado |
| D-20 | `sensor_id` explícitos + validación con degradación por tile | heurística por nombre de sensor | La heurística falla en silencio con valores equivocados. Los IDs explícitos fallan de forma visible y señalan qué re-mapear |
| D-21 | Umbrales `stale` 10 s / `dead` 60 s, con antigüedad visible | ocultar datos viejos; mostrarlos sin marca | Un dashboard que muestra datos viejos como si fueran actuales es peor que uno apagado |
| D-22 | Página de respaldo local en el teléfono cuando el PC está apagado | pantalla de error del navegador | Sin SIM, si el Wi-Fi o el PC caen el panel queda ciego; la página de error del navegador no comunica nada útil |
| D-23 | Host único, sin prefijos multi-host | `/api/hosts/{id}/...` desde el inicio | El día que haya varios PCs el problema real es descubrimiento y agregación, no la forma de la ruta. Un prefijo vacío hoy no lo ahorra |
| D-24 | Sin históricos ni base de datos | SQLite; Prometheus | El panel responde "¿cómo está ahora?". Las series temporales son otro producto |
| D-25 | GitHub Actions detrás de `CIProvider`, sin implementar | ambos proveedores en la v1 | Presupuesto de 10 h. La interfaz deja el hueco sin costo |
| D-26 | Metadatos de música aplazados a v2 | SMTC en la v1 | Leer título y artista exige la API `GlobalSystemMediaTransportControlsSession` (disponible desde Windows 10 1809) a través de un ayudante en .NET, porque no hay enlace directo cómodo desde Go. Es el 20 % del valor y el 40 % del esfuerzo. Las acciones de play/pausa y siguiente sí van en la v1, por teclas de medios |
| D-27 | `docker desktop status` con cadena de coincidencia configurable | parseo fijo de la salida | La cadena varía entre versiones de Docker Desktop. Un valor en configuración se corrige sin recompilar |
| D-28 | Secretos en `secrets.yaml` aparte, con ACL restringida | todo en `config.yaml`; variables de entorno | Permite versionar `config.example.yaml` completo sin riesgo, y acotar permisos al único archivo que los necesita |
| D-29 | Tres documentos en vez de cinco | README + GUIA_USUARIO + CLAUDE + ARCHITECTURE + DECISIONS | Para ~1.500 líneas de Go, cinco documentos se desincronizan antes de terminar la v1. El registro de decisiones vive aquí como §9, no como archivo huérfano |

---

## 10. Trabajo diferido

| Tema | Condición para abordarlo |
|---|---|
| Metadatos de música vía SMTC | la v1 lleva un mes estable |
| GitHub Actions | existe un pipeline real en GitHub que el usuario necesite vigilar |
| Acceso fuera de la LAN | Tailscale o WireGuard, nunca port forwarding; antes hay que comprobar que no choque con el cliente VPN de trabajo por tabla de rutas y DNS |
| TLS | solo si el modelo de amenaza de §8 cambia |
| Multi-host | cuando exista un segundo PC que lo justifique; implica descubrimiento y agregación, no solo cambiar rutas |
| Históricos | producto distinto; la respuesta probable es Grafana + Prometheus, no este binario |
