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

- Go **1.26.1**, `GOOS=windows`, `GOARCH=amd64`, `CGO_ENABLED=0`.
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
go build -o C:/Tools/calisto-panel/panel.exe ./cmd/panel
go run ./cmd/panel -config ./config.yaml
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
GET  /api/stream       → text/event-stream; eventos: hello, sensors, ci, actions
POST /api/do/{id}      → 202 {"exec":"...","action":"...","state":"running"}
                         401 unauthorized | 404 unknown_action
                         409 already_running | 429 rate_limited
```

Payload de ítem de sensor: `{id, label, value: float64, unit: string, state}`.
Payload de ítem de CI: `{id, label, state, result, build, finished, url}`.

`state` responde a "¿puedo confiar en este dato?" → `ok|stale|dead|unavailable|auth|error`.
`result` responde a "¿qué dice el dato?" y solo existe en CI.

El servidor envía valores **crudos**. El cliente formatea. El servidor nunca decide
unidades de presentación, colores ni texto de interfaz.

## Cadencias

- `sensors`: cada **5 s**.
- `ci`: cada **60 s**, configurable a **150 s**. Con backoff a 5 min tras error.
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
   `docker.start`, `docker.stop`, `vm.suspend`, `vm.start`, `media.playpause`,
   `media.next`. Nunca añadas un endpoint que ejecute un comando, ruta o argumento
   proveniente del cliente.
2. **`exec.Command` con argv fijo** armado desde `config.yaml`. Jamás `cmd /c`,
   `powershell -Command`, `sh -c` ni concatenación de cadenas en un comando.
3. El cliente envía **solo un ID de acción**. Sin parámetros, sin cuerpo significativo.
4. **Nunca registres ni devuelvas** el valor de `api_token` ni de `azure_devops_pat`,
   ni completos ni truncados, ni en errores, ni en `/healthz`, ni en modo depuración.
5. **El token nunca viaja en la URL** ni en query string. Solo en el encabezado
   `Authorization: Bearer`.
6. Los PAT **no salen del servidor**. El teléfono recibe estado derivado, nunca
   credenciales ni respuestas crudas del API de Azure.
7. `GET` sin autenticación; **todo `POST` exige** el bearer token. Comparación del token
   en tiempo constante (`crypto/subtle.ConstantTimeCompare`).
8. Límite de frecuencia en `POST`: máximo 1 acción cada 2 s en total, y una ejecución
   concurrente por ID de acción (de ahí el `409`).
9. Las acciones destructivas (`docker.stop`, `vm.suspend`) exigen **doble confirmación en
   el cliente**. Esa confirmación es de interfaz, no de protocolo: no inventes un
   parámetro `confirm` en la API.
10. El servicio corre **sin elevación**. Si algo parece necesitar privilegios de
    administrador, para y pregunta; no es la solución.
11. Nunca escribas archivos fuera de `%ProgramData%\CalistoPanel\`.
12. `vmrun`: solo `start <vmx> nogui` y `suspend <vmx> soft`. **`stop hard` está
    prohibido**: destruye trabajo sin guardar.

## Qué no hacer

- No añadas React, Vue, Angular, Tailwind, htmx, Alpine, ni `npm`, ni paso de build de
  frontend. El usuario tiene poca experiencia en frontend; la página plana es una
  decisión de diseño, no una limitación temporal.
- No introduzcas un router HTTP de terceros. `net/http` y `http.ServeMux` bastan.
- No agregues WebSockets. SSE es suficiente porque el flujo es unidireccional.
- No implementes TLS, ni certificados autofirmados, ni mTLS en la v1.
- No añadas base de datos, series temporales, gráficas ni históricos.
- No añadas prefijos multi-host (`/api/hosts/{id}/...`). El diseño es de host único a
  propósito.
- No implementes GitHub Actions todavía. Define `CIProvider` de forma que quepa, y para.
- No implementes metadatos de música (título, artista, portada). Las acciones
  `media.playpause` y `media.next` sí van en la v1; la lectura de SMTC es v2.
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
