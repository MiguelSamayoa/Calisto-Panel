# CLAUDE.md — Calisto Panel

Reglas obligatorias de este repositorio. Si algo aquí contradice una sugerencia tuya,
manda este archivo. Para profundidad (contratos completos, justificación de decisiones)
lee `docs/ARCHITECTURE.md`, y solo cuando la necesites.

## Qué es

Servicio Go en Windows 11 que sirve una página local a un teléfono Android en modo
kiosco: estado de pipelines de Azure DevOps, telemetría de hardware vía
LibreHardwareMonitor, y botones de acción sobre Docker Desktop, VMware Workstation y las
teclas de medios de Windows.

Host único. Red local. Sin TLS. Sin base de datos.

## Stack exacto

- Go **1.26.11**, `GOOS=windows`, `GOARCH=amd64`, `CGO_ENABLED=0`.
- Frontend: **un** `web/index.html` con `<style>` y `<script>` en línea, servido con
  `embed.FS`. Vanilla JS, CSS Grid, ES2017.
- Transporte de datos: Server-Sent Events.
- Dependencias permitidas, y **ninguna más sin preguntar**:
  - biblioteca estándar
  - `gopkg.in/yaml.v3`
  - `golang.org/x/sys/windows`
  - `gopkg.in/natefinch/lumberjack.v2` (rotación de logs)

## Estructura de carpetas

```
cmd/panel/main.go            arranque, flags, cableado
internal/config/             carga y validación de config.yaml y secrets.yaml
internal/lhm/                cliente HTTP, aplanado del árbol, parseo de valores
internal/ci/                 interfaz CIProvider
internal/ci/azuredevops/     implementación Azure DevOps
internal/actions/            registro, lista blanca, ejecutor
internal/hub/                hub SSE, estado en memoria, máquina de estados
internal/httpapi/            rutas, middleware de auth, límite de frecuencia
internal/winutil/            teclas de medios, ejecución de procesos
web/index.html               la página del panel
web/offline/index.html       página de respaldo que se copia al teléfono
testdata/lhm/data.json       fixture real de la máquina del usuario
config.example.yaml          plantilla; el real vive en %ProgramData%
```

Nada de `pkg/`. Nada de `utils/` genérico. Nada de paquetes de un solo archivo trivial.

## Comandos

```bash
go build -ldflags "-H=windowsgui" -o C:/Tools/calisto-panel/panel.exe ./cmd/panel
go run ./cmd/panel -config ./config.yaml   # con consola, para depurar
go test ./...
go vet ./...
golangci-lint run
gofmt -l .                  # debe salir vacío
```

Crea un `Makefile` con los objetivos `build`, `run`, `test`, `lint`, `fmt` que envuelvan
exactamente esos comandos. `make` no viene con Windows; los comandos de arriba son la
ruta canónica y el `Makefile` es una comodidad, no la fuente de verdad.

## Convenciones de código

- Identificadores, nombres de archivo, comentarios y mensajes de commit en **inglés**.
  Documentación en `docs/` y `README.md` en **español**.
- Commits en formato Conventional Commits: `feat:`, `fix:`, `refactor:`, `docs:`,
  `test:`, `chore:`.
- Errores envueltos con `fmt.Errorf("...: %w", err)`. Sin `panic` fuera de `main`.
- Sin `init()`. Las dependencias se inyectan desde `main`.
- Un `context.Context` por operación de red, siempre con timeout explícito.
- `log/slog` para todo el logging, en JSON hacia el archivo rotativo.
- Tipos exportados solo cuando cruzan un límite de paquete.
- Los *goroutines* de sondeo se detienen al cancelarse el contexto; sin fugas al cerrar.

## Contrato público (no romper sin actualizar `docs/ARCHITECTURE.md`)

```
GET  /                 → web/index.html
GET  /healthz          → 200 {"ok":true}
GET  /api/stream       → text/event-stream; eventos: hello, sensors, processes, docker, media, vm, ci, actions
GET  /api/media/cover?v=<id> → imagen de la portada actual | 404 not_found (sin auth)
GET  /api/logs/{proyecto}            → 200 {kind, name, containers, lines}   (exigen bearer;
GET  /api/logs/container/{id}          ver regla 7)  401 unauthorized | 404 not_found
POST /api/do/{id}      → 202 {"exec":"...","action":"...","state":"running"}
                         401 unauthorized | 404 unknown_action | 409 already_running
```

Payload de ítem de sensor: `{id, label, value: float64, unit: string, state}`.
Payload de ítem de CI: `{id, label, state, result, build, finished, url}`.
`processes` y `docker` son de lista dinámica: `state` va en el sobre, no en cada ítem.
Ítem de proceso: `{id, label, cpu, mem, count}`. Ítem de contenedor:
`{id, project, name, image, status, state, ports, cpu, mem, memLimit, pids}`. `media` y
`vm` son un único objeto en el sobre (no una lista). Detalle en `docs/ARCHITECTURE.md` §3.

`state` responde a "¿puedo confiar en este dato?" → `ok|stale|dead|unavailable|auth|error`.
`result` responde a "¿qué dice el dato?" y solo existe en CI.

El servidor envía valores **crudos**. El cliente formatea. El servidor nunca decide
unidades de presentación, colores ni texto de interfaz.

## Cadencias

- `sensors`: cada **5 s**.
- `ci`: cada **60 s**, configurable a **150 s**. Con backoff a 5 min tras error.
- `processes`: cada **5 s**. `docker`: cada **5 s**, solo si Docker está habilitado.
- `media`: cada **2 s**, y de nuevo a los ~0,25 s y ~0,7 s de cada acción de medios.
  `vm`: cada **5 s**, solo si VMware está habilitado.
- `actions`: solo como respuesta a una ejecución. Nunca por sondeo.
- Comentario `: keepalive` en el stream cada 15 s, y `retry: 3000`.

Nunca unifiques los dominios en un snapshot único a la cadencia de sensores. Consultar el
API de Azure DevOps cada 5 s provoca throttling y es el motivo por el que el diseño está
partido.

## Umbrales

`stale` a los 10 s. `dead` a los 60 s. Timeout de acción 60 s. Están en `config.yaml`;
no los dupliques como constantes en el código.

## Restricciones de seguridad — no negociables

1. **Lista blanca cerrada de acciones.** Los IDs válidos son exactamente:
   `docker.start`, `docker.stop`, `vm.stop`, `vm.start`, `media.playpause`,
   `media.prev`, `media.next`, `media.seek_back`, `media.seek_fwd` (±10 s), más la familia
   `media.volume.<n>` (D-34; `<n>` es un entero 0-100 en su forma canónica, sin signo ni ceros
   a la izquierda; solo fija el volumen maestro, nunca llega a un `argv`), más cuatro
   familias de Docker (D-31): `container.start.<id>`,
   `container.stop.<id>`, `project.start.<nombre>` y `project.stop.<nombre>`. `<id>` debe
   ser exactamente 12 dígitos hexadecimales en minúscula **y** figurar en la lista de
   contenedores que el propio servidor obtuvo en su último sondeo de `docker ps`;
   `<nombre>` debe ser un proyecto de Compose de esa misma lista (nunca entra al `argv`:
   el servidor usa los IDs que él mismo listó para ese proyecto). Si no, `404
   unknown_action`. Verbos: solo `start` y `stop`. Nunca añadas un endpoint que ejecute un
   comando, ruta o argumento proveniente del cliente; esas familias son la única
   excepción.
2. **`exec.Command` con argv fijo** armado desde `config.yaml`. Jamás `cmd /c`,
   `powershell -Command`, `sh -c` ni concatenación de cadenas en un comando.
3. El cliente envía **solo un ID de acción** (el sufijo de contenedor forma parte del ID).
   Sin parámetros, sin cuerpo significativo.
4. **Nunca registres ni devuelvas** el valor de `api_token` ni de `azure_devops_pat`,
   ni completos ni truncados, ni en errores, ni en `/healthz`, ni en modo depuración.
5. **El token nunca viaja en la URL** ni en query string. Solo en el encabezado
   `Authorization: Bearer`.
6. Los PAT **no salen del servidor**. El teléfono recibe estado derivado, nunca
   credenciales ni respuestas crudas del API de Azure.
7. `GET` sin autenticación **salvo `/api/logs/{proyecto}`**: los logs de contenedores suelen
   contener secretos, así que exigen el bearer igual que un `POST`. **Todo `POST` exige**
   el bearer token. Comparación del token en tiempo constante
   (`crypto/subtle.ConstantTimeCompare`).
8. **Sin límite de frecuencia global.** Una ejecución concurrente por ID de acción (de ahí
   el `409`); las de medios nunca se rechazan ni esperan. Evitar el doble disparo de un
   par encender/apagar es trabajo de la interfaz: al pulsar uno se bloquea y se desbloquea
   el contrario al instante. (Antes: 1 acción cada 2 s. `rate_limit_ms` del `config.yaml`
   se acepta y se ignora.)
9. Las acciones destructivas (`docker.stop`, `vm.stop`, `container.stop`, `project.stop`) exigen **doble confirmación en
   el cliente**. Esa confirmación es de interfaz, no de protocolo: no inventes un
   parámetro `confirm` en la API.
10. El servicio corre **sin elevación**. Si algo parece necesitar privilegios de
    administrador, para y pregunta; no es la solución.
11. Nunca escribas archivos fuera de `%ProgramData%\CalistoPanel\`.
12. `vmrun`: solo `start <vmx> nogui` y `stop <vmx> soft` (apagado ordenado del invitado,
    necesita VMware Tools), más `list` (solo lectura, para saber si la VM corre).
    **`stop hard` está prohibido**, y también `reset` y `suspend`: el botón de la VM apaga,
    y un apagado forzado destruye trabajo sin guardar.

## Qué no hacer

- No añadas React, Vue, Angular, Tailwind, htmx, Alpine, ni `npm`, ni paso de build de
  frontend. El usuario tiene poca experiencia en frontend; la página plana es una
  decisión de diseño, no una limitación temporal.
- No introduzcas un router HTTP de terceros. `net/http` y `http.ServeMux` bastan.
- No agregues WebSockets. SSE es suficiente porque el flujo es unidireccional.
- No implementes TLS, ni certificados autofirmados, ni mTLS en la v1.
- No añadas base de datos, series temporales ni históricos **en el PC**. Las gráficas del
  panel Sistema (1 o 5 minutos) sí existen, pero su historial vive solo en la memoria de la
  página y en una copia en el almacenamiento del teléfono (D-33); el servidor no guarda ni
  expone nada histórico. Nada de gráficas de más de 5 minutos.
- No añadas prefijos multi-host (`/api/hosts/{id}/...`). El diseño es de host único a
  propósito.
- No implementes GitHub Actions todavía. Define `CIProvider` de forma que quepa, y para.
- Metadatos de música: título, artista, posición y portada, leídos de SMTC con llamadas WinRT
  directas desde `internal/winutil` (sin cgo ni dependencias). La portada se guarda solo en memoria y se sirve en
  `GET /api/media/cover?v=<id>` (sin auth). El avance y
  retroceso de 10 s usan SMTC; el resto de teclas de medios, `SendInput`.
- No lo conviertas en servicio de Windows. En la sesión 0 las teclas de medios no llegan
  a ninguna aplicación.
- No hagas que `panel.exe` falle al arrancar si LHM no responde o si un `sensor_id` no
  existe. Degrada el tile afectado y sigue.
- No normalices ni formatees valores en el servidor.
- No subas `config.yaml` real, `secrets.yaml`, `*.log` ni binarios. `.gitignore` debe
  cubrirlos; `config.example.yaml` sí va al repo.
- No reintroduzcas sondeo del API de CI a la cadencia de sensores "para simplificar".

## Pruebas

Obligatorias:
1. Parser de LHM contra `testdata/lhm/data.json` (fixture real, usa punto decimal),
   incluyendo la regla de separador decimal (los casos con coma son cadenas sintéticas): `"41,0 °C"` → `41.0`, `"4.593,0 MHz"` → `4593.0`, `"1,000 V"` →
   `1.0`, y valores ausentes o `"-"`.
2. Resolución de acciones: ID desconocido rechazado, argv construido exactamente como lo
   define la configuración.
3. Autorización del `POST`: sin token, token incorrecto, token correcto.

No hay tests de interfaz. No hay CI en GitHub en la v1.

## Si falta información

El repositorio tiene seis valores específicos de la máquina del usuario (usuario de
GitHub, organización y pipelines de Azure DevOps, `SensorId` de LHM, subred, ruta de
`vmrun.exe`, cadena de `docker desktop status`). Están listados en §1 del `README.md`.
Si no están en `config.yaml`, **pregunta**; no inventes valores plausibles ni dejes
marcadores silenciosos en el código.
