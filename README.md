# Andén

Plataforma de contenedores con scale-to-zero sobre un datacenter de 3 nodos simulados en una sola máquina. Su eje de diseño es el **presupuesto de arranque en frío**: el tiempo desde la primera petición a un servicio dormido hasta su primer byte de respuesta.

Corre contenedores de proceso con un runtime propio sobre el kernel del host, duerme cualquier servicio TCP sin tráfico y lo despierta de forma transparente reteniendo la primera conexión. Compara dos rutas de despertar (relanzar y restaurar con CRIU) contra runtimes de referencia. Además coloca, despliega sin downtime, revierte y recoloca servicios ante la caída o partición de un nodo.

Cada resultado se mide con un cliente externo y contra una comparación externa, nunca contra una opinión.

## Estado por fase

| Fase | Contenido | Estado |
|---|---|---|
| K0 | Fundación: contrato, investigación y plan | Hecha |
| K1 | Runtime OCI y almacén de imágenes | Cerrada |
| K2 | Datacenter, API y despertar por relanzamiento | Cerrada |
| K3 | Restauración, desglose por fases, descubrimiento y comparación | Pendiente |
| K4 | Colocación, rollouts, caída y partición | Pendiente |
| K5 | Observabilidad, densidad y construcción | Pendiente |
| K6 | Diseño y publicación de resultados | Pendiente |

## Resultados

Medido el 6 nov 2026 en esta máquina (kernel 7.2.14, Ryzen 5 5600G). Los límites de cada cifra están en [`LIMITATIONS.md`](LIMITATIONS.md).

| Criterio | Comparación | Resultado medido |
|---|---|---|
| C3 | Subconjunto OCI de runtime-tools (`bd3b0bd`) | 20 de 20 pruebas pasan contra `anden-runtime`: 2220 aserciones `ok`, 0 `not ok` |
| C6 | `probe` en Andén y en runc 1.5.2 | Informes idénticos: PID 1 propio, hostname propio, procesos del host invisibles, `mount` y `swapon` con `EPERM` |
| C20 | Rootfs de umoci | Iguales en ruta, tipo, modo y hash en alpine (515 entradas), nginx (5014) y la imagen sintética (18) |
| C21 | Digest de cada blob | 7 de 7 blobs coinciden. Con un byte cambiado en una capa, `verify` la nombra y `unpack` sale con 1 |
| C22 | `du -sb` del almacén | nginx y nginx-perl comparten 7 capas, y cada capa se guarda una vez: 76 099 075 bytes, frente a 76 072 090 de capas únicas y 139 984 343 sin compartir |
| C1 | Cliente generado de la API publicada | `create`, `get`, `sleep`, `wake` y `delete` dan el código gRPC esperado, y consultar lo borrado da `NotFound`. Regenerar la API no cambia `gen/` |
| C2 | Estado antes y después de SIGKILL a `andend` | `anden status` y los procesos de las instancias quedan idénticos, con servicios en 3 nodos, y los 3 responden 200 |
| C4 | Otros servicios bajo carga | Con `memory.max` de 64 MiB, `oom_kill` da 1 y el servicio queda en `FAILED`; los otros dos atienden 1442 peticiones cada uno, 0 errores |
| C5 | Límites de la cgroup | `pids.max` 64: `pids.events` da `max 283`, `pids.current` 64 y el host sigue pudiendo hacer fork. Otros dos servicios: 2405 peticiones cada uno, 0 errores. Con cuota de 0.5 CPU, usa 0.501 CPU en 5 s |
| C7 | `cgroup.procs` y `memory.current` | Con `idle_seconds` 2, duerme a los 2.51 s, `cgroup.procs` queda vacío, `memory.current` da 512 000 bytes y despierta por `relaunch` |
| C8 tibia | Cliente externo; umbral de 1 s | n = 300, 0 errores: p50 52.2 ms, p90 58.6 ms, p99 63.3 ms, máximo 66.2 ms. Bajo 1 s aquí |
| C8 fría | Cliente externo, sin umbral | No medida: omitida con código 77 porque `sudo -n` no está disponible |
| C9 | Registro del cliente contra el del servicio | 32 clientes, 5 rondas: 1600 peticiones con respuesta y cada id una sola vez en el servicio |
| C10 | `redis-cli` contra Valkey, solo con `-h` y `-p` | 15 de 15 respuestas esperadas en 5 ciclos de sueño |
| C27 | Netns, cgroups, montajes y nftables antes y después | Las listas de antes y después de `anden up` y `anden down` son iguales y vacías |

De K3 en adelante todavía no hay mediciones.

## Uso

Requisitos, comandos y entorno se documentan al existir el producto. El chequeo del repositorio es:

```sh
make check
```

## Documentos

- [`vault/CONTRACT.md`](vault/CONTRACT.md): qué debe hacer el sistema y las decisiones cerradas.
- [`vault/RESEARCH.md`](vault/RESEARCH.md): fuentes y versiones verificadas.
- [`LIMITATIONS.md`](LIMITATIONS.md): lo que el proyecto no demuestra.
- [`CHANGELOG.md`](CHANGELOG.md): historial de cambios.
