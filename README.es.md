<p align="center">
  <img src="assets/icon-256.png" width="96" alt="">
</p>

<h1 align="center">Puente Eden ⇄ Switch</h1>

<p align="center"><a href="README.md">English</a> · <b>Español</b></p>

<p align="center">
  Juega en <b>modo local / inalámbrico</b> entre una <b>Nintendo Switch real con ldn_mitm</b> y el emulador <b>Eden</b> en tu PC.
</p>

<p align="center">
  <img alt="Windows" src="https://img.shields.io/badge/Windows-10%20%7C%2011-0078D4?logo=windows&logoColor=white">
  <img alt="Licencia GPL-3.0" src="https://img.shields.io/badge/licencia-GPL--3.0-blue">
  <img alt="Estado" src="https://img.shields.io/badge/estado-experimental-orange">
</p>

<p align="center">
  <img src="docs/jugando.jpg" width="760" alt="Ventana del Puente Eden ⇄ Switch con una partida en marcha">
</p>

---

## ¿Qué hace?

Eden tiene su propio multijugador local (salas en las que los emuladores se ven como si estuvieran en la misma red inalámbrica), y **ldn_mitm** hace que una Switch juegue en local a través del WiFi. Los dos funcionan de forma parecida, pero **no se entienden entre sí**.

Este programa hace de **traductor** entre ellos:

```
 ┌──────────────┐   WiFi de casa (ldn_mitm)   ┌────────────────────┐   sala de Eden   ┌────────┐
 │   Switch     │ ◀─────────────────────────▶ │  Puente Eden⇄Switch│ ◀──────────────▶ │  Eden  │
 │ con ldn_mitm │                             │   (en tu PC)       │                  │ (PC)   │
 └──────────────┘                             └────────────────────┘                  └────────┘
```

- Para **Eden**, la Switch es un jugador más de la sala.
- Para la **Switch**, el PC es otra consola con ldn_mitm.
- Funciona **en los dos sentidos**: la partida la puede crear la Switch o Eden.

Probado con:

- **Super Smash Bros. Ultimate** (13.0.1)
- **Mario Kart 8 Deluxe**

## Características

- 🪟 **Ventana con pasos guiados**: te dice qué hacer en cada momento y marca en verde lo que ya está listo.
- 🔎 **Detecta la Switch sola** y elige la red correcta del PC (aunque tengas VPN o adaptadores virtuales).
- 🏠 **Crea la sala de Eden** y le da a cada jugador su IP real, para que funcionen juegos que guardan las direcciones IP en sus propios datos (como Smash).
- 🛡️ **Configura el Firewall de Windows** con un clic.
- 📊 Muestra en directo los dispositivos conectados, el estado de la partida y los datos que pasan en cada sentido.
- 📦 Un solo `.exe`, sin instalación y sin dependencias.

## Requisitos

| | |
|---|---|
| **PC** | Windows 10 u 11 con Microsoft Edge (viene con Windows) |
| **Eden** | Con *Emulación › Configurar › Red* apuntando a tu adaptador de red |
| **Switch** | Con Atmosphère y **ldn_mitm** instalado y compatible con tu firmware |
| **Red** | Switch y PC conectados al **mismo router** |
| **Juego** | El mismo juego y la **misma versión** en la Switch y en Eden |

## Cómo se usa

1. **Descarga** `PuenteEdenSwitch.exe` desde la sección [Releases](../../releases) y ábrelo.
   Si Windows avisa de que no reconoce el programa: *Más información › Ejecutar de todas formas*.
2. **Eden**: si tienes una sala creada, ciérrala. Luego ve a **Multijugador › Conexión directa** y pon:

   | IP | Puerto |
   |---|---|
   | `127.0.0.1` | `24872` |

3. **Switch**: conéctala al mismo WiFi, abre el overlay con <kbd>L</kbd> + <kbd>↓</kbd> + <kbd>R3</kbd> y activa **ldn_mitm**.
4. **Juego**: abre el mismo juego en los dos, entra en el modo **local / inalámbrico**, crea la partida en uno y únete desde el otro.

La ventana del programa te guía por estos pasos y te avisa si falta algo:

<p align="center">
  <img src="docs/esperando.jpg" width="640" alt="El programa esperando a que Eden se conecte">
</p>


Deja la ventana abierta mientras jugáis. Al cerrarla se apaga el puente.

## Problemas frecuentes

<details>
<summary><b>"El puerto de la sala está ocupado"</b></summary>

Eden tiene una sala propia abierta. Ciérrala con *Multijugador › Abandonar sala* (o cierra Eden y vuelve a abrirlo). El programa lo reintenta solo.
</details>

<details>
<summary><b>La Switch no aparece</b></summary>

- Comprueba que ldn_mitm está en **ON** y que la Switch está conectada al **mismo router** que el PC.
- Entra en el modo local del juego: la Switch solo se anuncia cuando el juego busca o crea partidas.
- Si sale el aviso del Firewall en la parte de abajo, pulsa **Permitir en el Firewall**.
</details>

<details>
<summary><b>La Switch entra en la partida pero se sale a los pocos segundos</b></summary>

- Asegúrate de que la versión del juego es **exactamente la misma** en los dos.
- Usa la sala que crea el programa (Conexión directa a `127.0.0.1:24872`), no una sala creada en Eden.
</details>

<details>
<summary><b>Error de red en la Switch con ldn_mitm activado</b></summary>

Comprueba que tu versión de ldn_mitm es la que corresponde a tu firmware y a tu Atmosphère, y que la Switch se conecta a internet con ldn_mitm desactivado.
</details>

## Opciones avanzadas

El programa no necesita opciones, pero acepta estas (por ejemplo, en un acceso directo):

| Opción | Para qué sirve |
|---|---|
| `-switch 192.168.1.50` | Fija la IP de la Switch si no se detecta sola |
| `-ip 192.168.1.40` | Fija la IP del PC en tu red local |
| `-puerto-sala 24872` | Cambia el puerto de la sala |
| `-password texto` | Pone contraseña a la sala |
| `-nombre Switch` | Nombre con el que aparece la Switch en Eden |
| `-sala ip:puerto` | Se une a una sala de Eden ya creada en vez de crear una (modo antiguo) |
| `-consola` | Usa la consola en lugar de la ventana |

## Cómo funciona por dentro

- **Lado Switch**: habla el protocolo de red local de [ldn_mitm](https://github.com/spacemeowx2/ldn_mitm) (búsqueda por UDP y conexión por TCP en el puerto `11452`).
- **Lado Eden**: implementa el protocolo de salas de Eden/yuzu (sobre ENet), tanto como servidor de sala como cliente, y el reenvío de paquetes del juego (`ProxyPacket`, comprimidos con zstd).
- **Traducción**: la información de la partida (`NetworkInfo`, `NodeInfo`) pasa de un lado a otro, y los paquetes UDP del juego se reenvían entre la red de casa y la sala.
- La interfaz es una página web local (`127.0.0.1:24880`) que se abre en una ventana propia de Edge en modo aplicación.

## Compilar

Necesitas [Go](https://go.dev/dl/) 1.22 o superior.

```bash
# icono y manifiesto (genera rsrc_windows_amd64.syso)
go run ./tools/mkrsrc -o rsrc_windows_amd64.syso assets/icon-256.png assets/icon-64.png assets/icon-48.png assets/icon-32.png assets/icon-16.png

# programa con ventana
GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "-s -w -H windowsgui" -o PuenteEdenSwitch.exe .
```

En Windows también puedes usar `build.bat`. Cada vez que se publica una versión en *Releases*, GitHub Actions compila el `.exe` y lo adjunta automáticamente.

## Limitaciones

- De momento, **2 consolas**: una Switch y un Eden. Pero en cada lado pueden jugar **varias personas con sus propios mandos** (las que permita el juego por consola), por ejemplo varios amigos en el PC con Eden.
- Los juegos que usan **TCP** para la partida no funcionan, porque las salas de Eden no lo permiten.
- Es un proyecto **experimental**: no todos los juegos están probados.

## Créditos

- [ldn_mitm](https://github.com/spacemeowx2/ldn_mitm) de spacemeowx2 y la [rama mantenida por DefenderOfHyrule](https://github.com/DefenderOfHyrule/ldn_mitm).
- Eden, y antes yuzu y Citra, por el protocolo de salas y el multijugador local.
- [ENet](http://enet.bespin.org/), protocolo de red reimplementado aquí en Go.
- [klauspost/compress](https://github.com/klauspost/compress) para zstd.

## Aviso legal

Proyecto no oficial, sin relación con Nintendo ni con los desarrolladores de Eden o ldn_mitm. *Nintendo Switch* y *Super Smash Bros.* y *Mario Kart* son marcas de Nintendo. Este programa no incluye ni facilita juegos, firmware ni claves: úsalo solo con juegos que tengas legalmente.

## Licencia

[GPL-3.0-or-later](LICENSE).
