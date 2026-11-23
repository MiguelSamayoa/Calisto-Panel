# Calisto Panel

Panel auxiliar para PC con Windows, servido a un Redmi Note 10S en modo kiosco.
Un binario Go único expone una página local con estado de pipelines de Azure DevOps,
telemetría de hardware y botones de acción sobre Docker Desktop, VMware Workstation y
los controles de medios de Windows.

- **Audiencia de este documento:** el operador (tú).
- **Contexto técnico para agentes:** [`CLAUDE.md`](CLAUDE.md) y [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md).

---

## 1. Valores que debes completar

El repositorio no funciona recién clonado. Estos seis valores son específicos de tu
máquina y no pueden deducirse. Completa los seis antes de ejecutar nada.

| # | Valor | Dónde va | Cómo obtenerlo |
|---|---|---|---|
| 1 | Usuario de GitHub | `go.mod` (`module github.com/<GITHUB_USER>/calisto-panel`) | — |
| 2 | Organización, proyecto e IDs de definición de Azure DevOps | `config.yaml` → `ci.azure_devops` | §4.6 |
| 3 | `SensorId` de LibreHardwareMonitor | `config.yaml` → `sensors.items[].sensor_id` | §4.4 |
| 4 | Subred de tu LAN | regla de firewall | §4.8 |
| 5 | Ruta real de `vmrun.exe` | `config.yaml` → `actions.vmrun_path` | §4.3 |
| 6 | Cadena que imprime `docker desktop status` | `config.yaml` → `actions.docker.status_running_match` | §4.2 |

---

## 2. Cómo funciona, de extremo a extremo

```
┌─ PC Windows 11 ────────────────────────────────────────────────┐
│                                                                │
│  LibreHardwareMonitor (admin)                                  │
│    └─ HTTP 127.0.0.1:8085/data.json ──┐                        │
│                                        │ poll 5 s              │
│  Azure DevOps REST API ───────────────┤ poll 60 s o 150 s      │
│  (dev.azure.com, HTTPS, PAT)           │                        │
│                                        ▼                        │
│                            ┌───────────────────────┐            │
│                            │ panel.exe (Go 1.26.1) │            │
│                            │  estado en memoria    │            │
│                            └───────┬───────────────┘            │
│                                    │ 0.0.0.0:7171               │
│  docker desktop start|stop ◄───────┤                            │
│  vmrun start|suspend     ◄─────────┤  ejecuta acciones          │
│  teclas de medios (SendInput) ◄────┘  de lista blanca           │
└────────────────────────────────┬───────────────────────────────┘
                                 │ LAN, HTTP, perfil Privado
                                 ▼
                    ┌────────────────────────────┐
                    │ Redmi Note 10S             │
                    │ Fully Kiosk Browser (PLUS) │
                    │  GET /  → index.html       │
                    │  SSE /api/stream (lectura) │
                    │  POST /api/do/{id} (token) │
                    └────────────────────────────┘
```

Flujo en una frase: `panel.exe` consulta dos fuentes a cadencias distintas, mantiene el
último estado conocido en memoria y lo empuja al teléfono por Server-Sent Events; el
teléfono solo pinta y, con un token, dispara acciones de una lista cerrada.

**Lo que el panel nunca hace:** no guarda históricos, no tiene base de datos, no expone
los PAT al teléfono, no acepta comandos arbitrarios y no usa TLS en esta versión.

### Separación de lectura y escritura

| Superficie | Autenticación | Alcance |
|---|---|---|
| `GET /`, `GET /api/stream`, `GET /healthz` | ninguna | cualquiera en tu LAN puede ver métricas y estado de builds |
| `POST /api/do/{id}` | `Authorization: Bearer <token>` | solo quien tenga el token |

Esto es deliberado: tu requisito era que la familia pueda ver pero no ejecutar. Asume
que cualquier persona con acceso físico al teléfono encendido **tiene** el token, porque
vive en el `localStorage` de ese navegador. Por eso ninguna acción de la lista blanca es
irreversible y las dos destructivas exigen doble toque.

---

## 3. Prerrequisitos

| Componente | Versión verificada en tu equipo | Nota |
|---|---|---|
| Windows | 11, 26H2, compilación 26300.10078 | SendInput de teclas de medios requiere sesión interactiva |
| Go | 1.26.1 windows/amd64 | `CGO_ENABLED=0` |
| LibreHardwareMonitor | última estable | **requiere privilegios de administrador** para temperaturas y GPU |
| Docker Desktop | CLI plugin v0.4.3 presente | el subcomando `docker desktop` existe desde Docker Desktop 4.37 |
| VMware Workstation Pro | instalado | `vmrun.exe` se distribuye con Pro, no con Player |
| Fully Kiosk Browser | última | licencia PLUS necesaria para modo kiosco y salvapantallas (8.90 EUR / 10.99 USD, pago único por dispositivo) |

En el Redmi: Android 11 / MIUI 12.5.8. Sin SIM. Solo Wi-Fi.

---

## 4. Configuración paso a paso — Windows

### 4.1 Reserva DHCP

El panel apunta a una IP fija escrita en el teléfono. Sin reserva, el próximo reinicio
del router deja la pantalla en error permanente.

1. Obtén la MAC de tu adaptador Ethernet: `Get-NetAdapter | Select-Object Name, MacAddress`
2. En la administración del router, crea una reserva DHCP por esa MAC.
3. Verifica que Windows clasifique la red como **privada**:

```powershell
Get-NetConnectionProfile
# Si NetworkCategory dice Public:
Set-NetConnectionProfile -InterfaceAlias "Ethernet" -NetworkCategory Private
```

### 4.2 Docker Desktop

Verifica el comando y **copia su salida literal**:

```powershell
docker desktop status
```

La cadena que indica "en ejecución" varía entre versiones. Pon en `config.yaml`:

```yaml
actions:
  docker:
    status_running_match: "running"   # reemplaza con lo que viste arriba
```

El panel compara sin distinguir mayúsculas. Si esta cadena está mal, el tile de Docker
mostrará el estado invertido, no un error.

### 4.3 VMware Workstation

Verifica la ruta real:

```powershell
Get-ChildItem "C:\Program Files*\VMware\VMware Workstation\vmrun.exe"
```

Pon esa ruta en `actions.vmrun_path`. Comprueba que `vmrun` responde y que la VM
responde a suspensión suave:

```powershell
& "<ruta>\vmrun.exe" -T ws list
& "<ruta>\vmrun.exe" -T ws suspend "C:\Users\USUARIO\Documents\Virtual Machines\MI_VM\MI_VM.vmx" soft
```

`suspend ... soft` requiere **VMware Tools instalado en el huésped**. Si falla, instala
VMware Tools en MI_VM antes de habilitar el botón. El panel nunca usa `stop hard`.

El archivo `.vmxf` no es argumento válido; solo se usa `MI_VM.vmx`.

### 4.4 LibreHardwareMonitor y los SensorId

1. Descarga LibreHardwareMonitor y colócalo en `C:\Tools\LibreHardwareMonitor\`.
2. Ejecútalo **como administrador**. Sin elevación no verás temperaturas ni GPU.
3. `Options > Remote Web Server > Port` → `8085`.
4. `Options > Remote Web Server > Run` → activo.
5. Deja el *Listen IP* en `127.0.0.1`. **No** lo pongas en `0.0.0.0`: es un servicio con
   privilegios de administrador y sin autenticación por defecto, y no tiene por qué
   escuchar en la red. `panel.exe` lo consume desde la misma máquina.
6. Extrae los identificadores:

```powershell
Invoke-RestMethod http://127.0.0.1:8085/data.json |
  ConvertTo-Json -Depth 20 |
  Out-File -Encoding utf8 .\testdata\lhm\data.json
```

Abre ese archivo y busca los nodos hoja: cada uno tiene `SensorId`, `Text`, `Type` y
`Value`. Copia los seis que necesitas a `config.yaml`. Ejemplos de la forma que tienen
(**los tuyos serán distintos**):

```
/amdcpu/0/load/0              Type: Load          → uso de CPU
/amdcpu/0/temperature/0       Type: Temperature   → temperatura de CPU
/gpu-nvidia/0/load/0          Type: Load          → uso de GPU
/gpu-nvidia/0/temperature/0   Type: Temperature   → temperatura de GPU
/ram/data/0                   Type: Data          → memoria usada
/nvme/0/load/0                Type: Load          → uso de disco
```

Ese mismo archivo queda como fixture de pruebas. Guárdalo en el repositorio: es la
entrada de los tests del parser.

Al arrancar, `panel.exe` valida que cada `sensor_id` configurado exista. Si falta,
registra el error y marca **solo ese tile** como `unavailable`. No se cae.

### 4.5 Rutas de instalación

```
C:\Tools\calisto-panel\panel.exe            binario
C:\ProgramData\CalistoPanel\config.yaml     configuración (va al repo como ejemplo)
C:\ProgramData\CalistoPanel\secrets.yaml    PAT y token (NUNCA al repo)
C:\ProgramData\CalistoPanel\logs\panel.log  log rotativo, 5 archivos de 5 MB
```

```powershell
New-Item -ItemType Directory -Force "C:\Tools\calisto-panel", "C:\ProgramData\CalistoPanel\logs"
```

### 4.6 PAT de Azure DevOps

1. `https://dev.azure.com/<organización>/_usersSettings/tokens` → nuevo token.
2. Scope: **Build (Read)** únicamente. Nada más.
3. Caducidad: 90 días o menos. Anótate la fecha; cuando caduque el tile mostrará `auth`.
4. Obtén los IDs de definición de tus pipelines:

```powershell
$org = "<organización>"; $proj = "<proyecto>"; $pat = "<PAT>"
$h = @{ Authorization = "Basic " + [Convert]::ToBase64String([Text.Encoding]::ASCII.GetBytes(":$pat")) }
Invoke-RestMethod -Headers $h "https://dev.azure.com/$org/$proj/_apis/build/definitions?api-version=7.1" |
  Select-Object -ExpandProperty value | Select-Object id, name
```

5. Pon organización, proyecto y hasta 5 IDs en `config.yaml`; el PAT en `secrets.yaml`.

### 4.7 Secretos y permisos de archivo

`secrets.yaml`:

```yaml
api_token: "<cadena aleatoria larga>"     # el que escribirás en el teléfono
azure_devops_pat: "<PAT>"
```

Genera el token:

```powershell
[Convert]::ToBase64String((1..32 | ForEach-Object { Get-Random -Max 256 }))
```

Restringe el acceso:

```cmd
icacls "C:\ProgramData\CalistoPanel\secrets.yaml" /inheritance:r /grant:r "%USERNAME%:(R,W)" /grant:r "Administrators:(F)"
```

### 4.8 Firewall

```powershell
New-NetFirewallRule -DisplayName "Calisto Panel 7171" `
  -Direction Inbound -Action Allow -Protocol TCP -LocalPort 7171 `
  -Profile Private -RemoteAddress 192.168.1.0/24   # ← tu subred real
```

**Nunca habilites el perfil `Public` en esta regla.** Cuando conectes una VPN de
trabajo, Windows registra una interfaz nueva que puede clasificarse como pública; una
regla con `Public` expondría el panel en la red del cliente. El precio de no hacerlo es
que el panel deja de responder mientras la VPN está conectada; ese es el
comportamiento correcto.

No abras puertos en el router. Si algún día necesitas acceso remoto, se resuelve con
Tailscale o WireGuard, no con port forwarding.

### 4.9 Compilar y ejecutar

```powershell
cd <repo>
go build -o C:\Tools\calisto-panel\panel.exe ./cmd/panel
C:\Tools\calisto-panel\panel.exe -config C:\ProgramData\CalistoPanel\config.yaml
```

Verifica, en este orden:

```powershell
Invoke-RestMethod http://127.0.0.1:7171/healthz          # {"ok":true}
curl.exe -N http://127.0.0.1:7171/api/stream             # eventos cada 5 s, Ctrl+C para salir
```

Desde el teléfono, en el navegador normal: `http://<IP-del-PC>:7171/`. Si el `healthz`
local responde y el teléfono no, el problema es la regla de firewall o el perfil de red,
no el servicio.

### 4.10 Arranque automático

Dos tareas, con privilegios distintos a propósito.

```cmd
schtasks /Create /TN "LibreHardwareMonitor" /TR "\"C:\Tools\LibreHardwareMonitor\LibreHardwareMonitor.exe\"" /SC ONLOGON /RL HIGHEST /RU USUARIO /F

schtasks /Create /TN "CalistoPanel" /TR "\"C:\Tools\calisto-panel\panel.exe\" -config \"C:\ProgramData\CalistoPanel\config.yaml\"" /SC ONLOGON /RL LIMITED /RU USUARIO /F
```

`panel.exe` corre **sin elevación**: no la necesita para leer `localhost`, ejecutar
`docker desktop`, `vmrun` ni enviar teclas de medios. Menos privilegio en el proceso
expuesto a la red es la decisión correcta.

Tampoco se instala como servicio de Windows. Un servicio vive en la sesión 0 y desde ahí
las teclas de medios no llegan a ninguna aplicación. Debe correr en tu sesión
interactiva.

El orden de arranque no está garantizado. `panel.exe` tolera que LHM aún no esté listo:
reintenta y mantiene los tiles de hardware en `unavailable` hasta que responda.

---

## 5. Configuración paso a paso — Redmi Note 10S

### 5.1 MIUI: evitar que el sistema mate el panel

MIUI cierra aplicaciones en segundo plano de forma agresiva. Aplica los tres ajustes:

1. **Autoarranque:** app Seguridad → Permisos → Autoarranque → activa Fully Kiosk.
2. **Batería:** Seguridad → Batería → Ahorro de batería por aplicación → Fully Kiosk →
   **Sin restricciones**.
3. **Pantalla activa:** Opciones de desarrollador → *Permanecer activo* (mientras carga).

El truco de bloquear la app en la pantalla de recientes con el candado **puede no existir
en MIUI 12.5**: en esa versión la pulsación larga pasó a mostrar la información de la app
en lugar del menú con el candado. No cuentes con esa mitigación; los tres pasos de arriba
son los que importan.

A tu favor: con la pantalla encendida y el equipo cargando, el modo Doze no interviene.
El enemigo es el gestor de MIUI, no la suspensión.

### 5.2 Fully Kiosk

Instala Fully Kiosk Browser, activa la licencia PLUS y configura:

| Ajuste | Valor | Por qué |
|---|---|---|
| Start URL | `http://<IP-del-PC>:7171/` | — |
| Keep Screen On | activado | el panel debe estar visible siempre |
| Screen Brightness | bajo (20–35 %) | mitiga burn-in y calor |
| Kiosk Mode (PLUS) | activado, con PIN de salida | evita que alguien salga de la app |
| Screensaver (PLUS) | activado, de noche, con pantalla en negro | **mitigación principal de burn-in** |
| Reload on connection restore | activado | recupera solo al volver el Wi-Fi |
| URL de error (Web Content Settings) | página local de respaldo | ver §5.3; el nombre exacto del ajuste varía según la versión |

Desactiva en el teléfono: rotación automática, bloqueo de pantalla, y actualizaciones
automáticas de MIUI.

### 5.3 Página de respaldo cuando el PC está apagado

Sin esto, el teléfono muestra la pantalla de error del navegador. Copia
`web/offline/index.html` del repositorio al almacenamiento del teléfono y apúntalo como
URL de error en Fully Kiosk. Reintenta la Start URL periódicamente por sí sola.

### 5.4 Entregar el token

Abre el panel y pulsa el engranaje. Escribe el token de `secrets.yaml` una sola vez;
queda en `localStorage`. Hasta que lo hagas, los botones aparecen deshabilitados y la
lectura funciona normal.

El token **nunca** va en la URL de arranque de Fully Kiosk. Terminaría en el historial,
en los logs y en cualquier encabezado `Referer`.

### 5.5 Dedicación del dispositivo

Android 11 con MIUI 12.5.8 ya no recibe parches de seguridad y va a quedar
permanentemente en tu LAN. Úsalo solo para esto: sin cuenta de correo, sin navegación,
sin sesiones abiertas de nada.

---

## 6. Operación

### 6.1 Estados que verás en los tiles

| Estado | Significado | Umbral |
|---|---|---|
| `ok` | dato fresco | — |
| `stale` | el dato tiene más de 10 s; se muestra en gris con su antigüedad | 10 s |
| `dead` | el dato tiene más de 60 s; tile tachado | 60 s |
| `unavailable` | la fuente no responde o el `sensor_id` no existe | — |
| `auth` | el PAT fue rechazado (401/403) — caducó o perdió el scope | — |
| `error` | fallo de red u otro error de la fuente | — |

Un tile en `auth` significa "renueva el PAT", no "no hay builds". La distinción es
intencional: un dashboard que miente en silencio es peor que uno apagado.

Cuando LHM cae, **solo** los tiles de hardware pasan a `unavailable`. El CI sigue
funcionando, y al revés.

### 6.2 Comandos de diagnóstico

```powershell
# ¿vive el servicio?
Invoke-RestMethod http://127.0.0.1:7171/healthz

# ¿qué está emitiendo?
curl.exe -N http://127.0.0.1:7171/api/stream

# ¿responde LHM?
Invoke-RestMethod http://127.0.0.1:8085/data.json | ConvertTo-Json -Depth 5

# ¿sirve el PAT?
Invoke-RestMethod -Headers $h "https://dev.azure.com/$org/$proj/_apis/build/builds?api-version=7.1&`$top=1"

# log
Get-Content C:\ProgramData\CalistoPanel\logs\panel.log -Tail 50 -Wait

# ¿quién escucha en 7171?
Get-NetTCPConnection -LocalPort 7171 | Format-Table -AutoSize

# ¿aplica la regla de firewall?
Get-NetFirewallRule -DisplayName "Calisto Panel 7171" | Get-NetFirewallPortFilter
```

### 6.3 Fallos frecuentes

| Síntoma | Causa más probable | Comprobación |
|---|---|---|
| El teléfono no carga nada, `healthz` local sí responde | perfil de red en `Public`, o VPN conectada | `Get-NetConnectionProfile` |
| Todo cargaba y de golpe dejó de cargar | cambió la IP del PC (sin reserva DHCP) | `ipconfig` y comparar con la Start URL |
| Tiles de hardware en `unavailable`, CI bien | LHM cerrado, sin elevación, o servidor web apagado | abrir `http://127.0.0.1:8085/data.json` |
| Temperaturas y GPU ausentes, el resto bien | LHM corriendo **sin** privilegios de administrador | relanzar elevado |
| Valores numéricos absurdos (`41` en lugar de `41,0`, o `1` en lugar de `1000`) | regla de separador decimal mal aplicada al locale | §7 y el test del parser |
| Tile de CI en `auth` | PAT caducado o sin scope Build (Read) | regenerar el PAT |
| Tile de CI en `error` tras funcionar días | throttling de Azure DevOps | subir `ci.interval` a 150 s |
| Botones deshabilitados | el token no está en `localStorage` de este navegador | §5.4 |
| `429` al pulsar un botón | límite de frecuencia de acciones | esperar; es el comportamiento esperado |
| El panel desaparece tras unas horas | MIUI mató Fully Kiosk | §5.1, los tres ajustes |
| `vm.suspend` falla siempre | falta VMware Tools en MI_VM | §4.3 |
| Las teclas de medios no hacen nada | `panel.exe` no corre en tu sesión interactiva | §4.10 |

### 6.4 Mantenimiento con calendario

| Cada | Tarea |
|---|---|
| 90 días | renovar el PAT de Azure DevOps |
| al actualizar LHM | re-extraer `data.json` y verificar que los `SensorId` siguen existiendo |
| al actualizar Docker Desktop | re-verificar la salida de `docker desktop status` |
| cada 6 meses | revisar el panel con fondo claro para detectar burn-in incipiente |
| cada 6 meses | inspeccionar físicamente la batería del teléfono |

### 6.5 Riesgos de hardware aceptados

Estas dos cosas no tienen solución en software y las estás aceptando a conciencia:

- **Burn-in.** El Note 10S es AMOLED y el panel es estático. El salvapantallas nocturno
  y el brillo bajo lo retrasan; no lo evitan.
- **Batería.** Permanentemente al 100 % y con calor, en 1–2 años se degrada y puede
  hincharse. MIUI 12.5 no tiene límite de carga nativo. Si quieres mitigarlo de verdad,
  pon el cargador en un enchufe inteligente con horario. Una batería hinchada en un
  equipo sobre tu escritorio no es un problema estético.

---

## 7. Nota sobre el parseo de valores

LibreHardwareMonitor devuelve los valores como **cadenas con unidad y con el separador
decimal del locale del sistema**. En tu equipo (es-GT) verás `"41,0 °C"` y `"1,000 V"`,
donde la coma es decimal. En otro locale el mismo sensor llega como `"41.0 °C"`.

La regla implementada es: **el último separador presente en la cadena es el decimal**;
cualquier otro es separador de miles y se descarta. `"4.593,0 MHz"` → `4593.0`.
`"1,000 V"` → `1.0`.

Es el punto más frágil del proyecto y la causa de la fila "valores absurdos" en §6.3. El
fixture de `testdata/lhm/data.json` debe ser la salida real de tu máquina, no un ejemplo
de otra, o los tests validarán algo que no te ocurre. Las versiones recientes de LHM
añadieron un campo de unidad fija en `data.json`; el parser lo usa si está presente y
cae a la regla anterior si no.

---

## 8. Alcance de la v1

**Incluido:** 6 tiles (CI, CPU, RAM, GPU/temperatura, disco, red), 6 acciones
(`docker.start`, `docker.stop`, `vm.suspend`, `vm.start`, `media.playpause`,
`media.next`), SSE, token, arranque automático, firewall, Fully Kiosk configurado.

**Excluido a propósito, documentado en `docs/ARCHITECTURE.md`:** metadatos de música
(título y artista vía SMTC), GitHub Actions, TLS, Tailscale, históricos y gráficas,
soporte multi-PC, cualquier framework de frontend.
