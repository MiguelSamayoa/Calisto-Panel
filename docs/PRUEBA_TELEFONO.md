# Prueba en el teléfono — paso a paso

Guía para ver el panel por primera vez en el Redmi Note 10S. Está pensada para hacerse
**en orden**: cada fase comprueba algo que la siguiente necesita, así que si una falla se
sabe exactamente dónde mirar.

Estado actual: el panel funciona de extremo a extremo en el PC (probado en un navegador de
escritorio con tus sensores reales). **Nunca se ha probado en el teléfono.** Esta guía es
esa primera prueba.

Referencias: configuración detallada en [`README.md`](../README.md), contratos en
[`ARCHITECTURE.md`](ARCHITECTURE.md).

---

## Antes de empezar

| Necesitas | Cómo comprobarlo |
|---|---|
| LibreHardwareMonitor abierto **como administrador**, con el servidor web activo en `127.0.0.1:8085` | `Invoke-RestMethod http://127.0.0.1:8085/data.json` devuelve datos |
| `C:\ProgramData\CalistoPanel\secrets.yaml` con tu `api_token` | `Test-Path C:\ProgramData\CalistoPanel\secrets.yaml` → `True` |
| `config.yaml` de la raíz del repo (ignorado por git) | `Test-Path .\config.yaml` → `True` |
| El teléfono y el PC en la **misma red Wi-Fi/LAN** | el teléfono no debe estar en datos móviles ni en una red de invitados |
| Una VPN **desconectada** en esta primera prueba | se prueba con VPN en la fase 7 |

---

## Fase 1 — Compilar y arrancar en el PC

En PowerShell, dentro de la carpeta del repo:

```powershell
New-Item -ItemType Directory -Force C:\Tools\calisto-panel | Out-Null
go build -o C:\Tools\calisto-panel\panel.exe ./cmd/panel
C:\Tools\calisto-panel\panel.exe -config .\config.yaml
```

La ventana queda ocupada mientras el panel corre; es lo esperado. No cierres esa ventana.
Para detenerlo, `Ctrl+C`.

**Si Windows muestra un aviso de firewall** pidiendo permitir `panel.exe`: **desmarca
"Redes públicas"** y, si dudas, pulsa Cancelar. La regla explícita de la fase 3 es la que
manda.

En **otra** ventana de PowerShell:

```powershell
Invoke-RestMethod http://127.0.0.1:7171/healthz          # {"ok":true}
curl.exe -N --max-time 7 http://127.0.0.1:7171/api/stream  # hello, sensors, ci...
```

✅ Debes ver `retry: 3000`, un evento `hello` con 6 acciones y un evento `sensors` con tus
5 valores en estado `ok`.

❌ Si `sensors` sale con `state` distinto de `ok`:
- `unavailable` en todos → LHM no está corriendo o el servidor web está apagado.
- `unavailable` solo en uno → ese `sensor_id` no existe; mira el log (fase 2).

---

## Fase 2 — Ver el log

```powershell
Get-Content C:\ProgramData\CalistoPanel\logs\panel.log -Tail 20
```

✅ Debe haber una línea `"panel started"` con `"listen":"0.0.0.0:7171"`.

Cualquier `"sensor_id not found in lhm data"` indica qué tile tiene el ID mal.

---

## Fase 3 — IP del PC y firewall

### 3.1 IP del PC

```powershell
ipconfig
```

Anota la **Dirección IPv4 de la Ethernet** (en tu última lectura era `<IP_DEL_PC>`). Esa
es la IP que escribirás en el teléfono. Si usas el PC por Wi-Fi, usa la IPv4 del adaptador
Wi-Fi.

> La IP debe ser estable: haz la **reserva DHCP** en el router (README §4.1). Sin ella,
> la URL del teléfono deja de funcionar el día que el router reparta otra dirección.

### 3.2 Perfil de red

```powershell
Get-NetConnectionProfile
```

✅ `NetworkCategory` debe ser `Private`. Si dice `Public`:

```powershell
Set-NetConnectionProfile -InterfaceAlias "Ethernet" -NetworkCategory Private
```

### 3.3 Regla de firewall

En **PowerShell como administrador** (es lo único de esta guía que lo necesita):

```powershell
New-NetFirewallRule -DisplayName "Calisto Panel 7171" `
  -Direction Inbound -Action Allow -Protocol TCP -LocalPort 7171 `
  -Profile Private -RemoteAddress 192.168.1.0/24
```

Cambia la subred si la tuya no es `192.168.1.0/24`. **Nunca** añadas el perfil `Public`
(README §4.8).

Comprueba la regla:

```powershell
Get-NetFirewallRule -DisplayName "Calisto Panel 7171" | Get-NetFirewallPortFilter
```

---

## Fase 4 — Abrir el panel desde el teléfono

Primero con el **navegador normal** del teléfono (Chrome), no con Fully Kiosk. Si algo
falla, es mucho más fácil de diagnosticar así.

1. Con el teléfono en la misma Wi-Fi, abre: `http://<IP_DEL_PC>:7171/` (tu IP de la
   fase 3.1).
2. Chrome puede ofrecerte "Cambiar a HTTPS": ignóralo. El panel es HTTP a propósito.

✅ Debes ver el panel: punto verde arriba a la izquierda, 5 tiles con valores, "Sin
pipelines configurados" y 6 botones deshabilitados (aún no hay token).

❌ Si la página no carga:

| Síntoma | Causa probable | Qué hacer |
|---|---|---|
| Tiempo de espera agotado | firewall o perfil `Public` | repite la fase 3.2 y 3.3 |
| Tiempo de espera, y en el PC todo bien | el router aísla dispositivos (Wi-Fi de invitados, "AP isolation") | usa la red principal |
| "No se puede acceder" al instante | IP equivocada o el panel no está corriendo | `ipconfig` otra vez; revisa la fase 1 |
| Carga en el PC pero no en el teléfono | VPN activa en el PC | desconéctala y reintenta (fase 7) |

Prueba rápida de red desde el PC, sin teléfono: `Test-NetConnection <IP_DEL_PC> -Port 7171`
debe dar `TcpTestSucceeded : True`.

---

## Fase 5 — Introducir el token y probar una acción segura

1. En el PC, copia el token: `notepad C:\ProgramData\CalistoPanel\secrets.yaml` (es el
   valor entre comillas de `api_token`). **No lo pegues en ningún chat ni lo pongas en la
   URL.**
2. En el teléfono toca el engranaje ⚙, pega el token y pulsa **Guardar**. Los botones se
   habilitan.

**Primera acción: reproducir/pausar medios.** Es inofensiva y reversible. Abre antes algo
de música o un vídeo en el PC.

3. Toca el botón ⏯. En el PC debe pausarse o reanudarse el contenido. Tócalo otra vez para
   revertirlo.

✅ El botón muestra `…` y luego `✓`.

❌ Si dice "Token inválido": el token no coincide; vuelve a copiarlo sin espacios.
❌ Si dice `✓` pero no pasa nada en el PC: `panel.exe` no corre en tu sesión interactiva
(README §4.10). Arráncalo desde tu propia ventana de PowerShell, no como servicio.

4. Toca ⏭ (siguiente pista) si tienes una lista de reproducción.

### Probar la confirmación de acciones destructivas (sin ejecutarlas)

5. Toca **Docker ■** una vez. El botón debe pasar a **"¿Confirmar?"** en naranja. **No
   lo toques otra vez**: espera 3 segundos y vuelve a su aspecto normal. Lo mismo con
   **VM ⏸**.

> Pulsar el segundo toque de `Docker ■` detiene Docker Desktop, y el de `VM ⏸` suspende
> la máquina `MI_VM`. Hazlo solo cuando quieras de verdad; ambos son reversibles
> (`Docker ▶` y `VM ▶`), pero `VM ⏸` requiere **VMware Tools instalado en el huésped**
> (README §4.3).

### Acciones de Docker y VM (cuando quieras)

6. `Docker ▶` / `Docker ■`: inicia y detiene Docker Desktop. Tarda decenas de segundos;
   el botón queda en `…` hasta terminar.
7. `VM ▶` / `VM ⏸`: arranca (sin ventana) y suspende `MI_VM`.

En el log verás una línea `"action finished"` por cada ejecución, con el resultado y la
duración.

---

## Fase 6 — Probar los fallos (importante)

El panel debe comportarse bien cuando algo se cae. Comprueba cada uno:

| Qué haces | Qué debes ver en el teléfono |
|---|---|
| Cierra LibreHardwareMonitor | a los ~10 s los tiles se ponen grises con "dato viejo · N s"; a los 60 s se tachan ("sin datos") |
| Vuelve a abrir LHM (como administrador) | los tiles recuperan el color en pocos segundos |
| Detén `panel.exe` (`Ctrl+C`) | punto rojo, aviso "Sin conexión con el PC. Reintentando…" y tiles atenuados |
| Vuelve a arrancar `panel.exe` | el panel se recupera solo en ~3 s |
| Toca un botón sin token (borra el token en ⚙ → Borrar) | los botones están deshabilitados |
| Toca el mismo botón dos veces seguidas | "Espera 2 s" en el aviso inferior |

Un tile gris **no** es un error: significa que el dato es viejo. Es el comportamiento
diseñado (README §6.1).

---

## Fase 7 — Con VPN

Con el panel corriendo y la página abierta en el teléfono:

1. Conecta FortiClient (o WireGuard) en el PC.
2. Mira el teléfono unos 20 segundos.

✅ Lo normal es que siga funcionando.

❌ Si el teléfono se queda sin conexión solo con la VPN, es la VPN la que corta o
redirige tu LAN. Diagnóstico en el PC, con la VPN conectada:

```powershell
route print 192.168.1.*                       # ¿más de una ruta para tu subred?
Test-NetConnection <IP_DEL_ROUTER>                # ¿alcanzas el router?
Invoke-RestMethod http://<IP_DEL_PC>:7171/healthz
```

Causas y soluciones: README §4.1.1 (colisión de subred, cortafuegos de WireGuard con
`0.0.0.0/0`, política del administrador de FortiClient).

---

## Fase 8 — Pasar a Fully Kiosk

Cuando todo lo anterior funcione en el navegador normal:

1. Configura Fully Kiosk según README §5.2 (Start URL = `http://<IP-del-PC>:7171/`).
2. Copia `web/offline/index.html` al teléfono y ponlo como URL de error (README §5.3).
3. Aplica los tres ajustes de MIUI (README §5.1) para que el sistema no mate la app.
4. **El token se vuelve a introducir** en Fully Kiosk: `localStorage` es por navegador,
   no se comparte con Chrome.

✅ Prueba la página de respaldo: detén `panel.exe` y comprueba que Fully Kiosk muestra
"Sin conexión con el PC" y, al arrancar el panel de nuevo, vuelve solo.

---

## Fase 9 — Arranque automático (cuando todo funcione)

Solo después de las fases anteriores (README §4.10):

```cmd
schtasks /Create /TN "LibreHardwareMonitor" /TR "\"C:\Tools\LibreHardwareMonitor\LibreHardwareMonitor.exe\"" /SC ONLOGON /RL HIGHEST /RU USUARIO /F

schtasks /Create /TN "CalistoPanel" /TR "\"C:\Tools\calisto-panel\panel.exe\" -config \"C:\ProgramData\CalistoPanel\config.yaml\"" /SC ONLOGON /RL LIMITED /RU USUARIO /F
```

Antes de eso, copia la configuración a su sitio definitivo:

```powershell
Copy-Item .\config.yaml C:\ProgramData\CalistoPanel\config.yaml
```

`panel.exe` corre **sin elevación** y **no** como servicio de Windows (README §4.10).

---

## Checklist final

- [ ] `healthz` responde y el stream emite `sensors` con 5 valores `ok`
- [ ] El panel carga desde el navegador del teléfono
- [ ] El token se guardó y los botones se habilitaron
- [ ] ⏯ pausa/reanuda en el PC
- [ ] Los botones destructivos piden confirmación y la ventana expira sola
- [ ] Cerrar LHM degrada los tiles; reabrirlo los recupera
- [ ] Detener el panel muestra el aviso de conexión; arrancarlo lo recupera
- [ ] Con VPN conectada el panel sigue respondiendo (o ya sabes por qué no)
- [ ] La reserva DHCP del PC está hecha
- [ ] Fully Kiosk con la página de respaldo y los ajustes de MIUI

## Si encuentras un problema

Anota **qué fase**, **qué viste** y las últimas líneas del log
(`Get-Content C:\ProgramData\CalistoPanel\logs\panel.log -Tail 30`). El log nunca contiene
el token. No pegues el contenido de `secrets.yaml`.
