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
| K3 | Restauración, desglose por fases, descubrimiento y comparación | Cerrada |
| K4 | Colocación, rollouts, caída y partición | Cerrada |
| K5 | Observabilidad, densidad y construcción | Cerrada |
| K6 | Diseño y publicación de resultados | Pendiente |

## Resultados

Medido el 6 y 7 nov 2026 en esta máquina (kernel 7.2.14, Ryzen 5 5600G). Los límites de cada cifra están en [`LIMITATIONS.md`](LIMITATIONS.md).

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
| C11 | `dig` desde el cliente externo | `web.anden.internal` da `10.81.0.2` en node1; tras dormirlo, moverlo a node2 y despertarlo da `10.82.0.2`, y la conexión a esa dirección funciona |
| C12 | Suma de las 7 fases contra `wakeclient` | 100 despertares por ruta: 200 de 200 dentro de máx(5 ms, 10 %), en `relaunch` y en `restore` |
| C13 | runc 1.5.2, crun y `criu restore` directo, n = 100 por fila | p50 / p90 / p99 / máx en ms. `anden-relaunch` 55.3 / 62.2 / 66.6 / 66.9 y `anden-restore` 33.6 / 37.0 / 40.0 / 40.5. `runc` 38.0 / 38.9 / 39.3 / 60.2, `crun` 46.7 / 47.8 / 79.3 / 79.3 y `criu-restore` 16.8 / 17.2 / 17.5 / 17.5. `relaunch` frente a runc y crun: comparable; `restore` frente a `criu restore`: equivalente en el mecanismo |
| C14 | Petición inyectada mientras el servicio se duerme | 100 por ruta, 0 errores y 0 timeouts; 97 en `relaunch` y 96 en `restore` cayeron dentro de la transición |
| C15 | Fuerza bruta de `placeopt`, semillas 1 a 50, n de 6 a 12 | 0 violaciones de capacidad o anti-afinidad en 50 instancias. El scheduler coloca 289 servicios y la fuerza bruta 362: brecha de 73, máximo 4 por instancia, óptimo alcanzado en 9 de 50. Conclusión: equivalente (mismo problema y objetivo; la brecha es real) |
| C16 | vegeta a 200 peticiones/s durante 30 s, overlap 5 s, draining 2 s | 6000 peticiones, 0 errores. El cambio a v2 se ve 0.98 s después del despliegue y, tras el overlap, 50 de 50 muestras responden v2 |
| C17 | vegeta a 200 peticiones/s durante 30 s; v3 con healthcheck que falla | 6000 peticiones, 0 errores. v3 arrancó y recibió 0 peticiones; v2 sigue activa para la API y en 20 de 20 muestras del borde |
| C18 | Sonda externa cada 100 ms tras `anden chaos kill` | Los dos servicios de node1 vuelven en node2 con 4.70 s sin servicio cada uno, frente al límite de 10 s |
| C19 | Latidos del singleton en los tres nodos durante una partición de 7.02 s | 2 escritores en 258 latidos, sin solape: el primero deja de escribir 1.91 s después de la partición y el segundo empieza 4.55 s después, con 2.645 s sin escritor |
| C23 | `promtool check metrics` sobre `10.80.0.1:9090/metrics` | Sale con 0. Están las 7 fases de `anden_wake_phase_seconds` en `relaunch` y `restore`, `anden_wake_duration_seconds` de ambas rutas y `anden_node_up` en 1 para los 3 nodos |
| C24 | `promtool check rules` y `promtool test rules` | Ambos salen con 0 sobre `ops/alerts.yml`, con casos que disparan y casos sanos |
| C25 | Runbooks en `ops/runbooks/` y el uso de `bin/anden` | `AndenNodeDown` y `AndenWakeOverBudget` tienen su runbook, con sus encabezados y solo comandos que existen |
| C26 | `memory.current` del scope y `wakeclient`, 50 despertares por escalón | `echosvc` de 50 milicores y 32 MiB por `relaunch`, 0 errores. Dormidos / p99 de despertar / memoria por dormido: 10 / 251.8 ms / 4012.4 KiB, 50 / 317.9 ms / 1072.2 KiB, 100 / 249.5 ms / 639.6 KiB, 250 / 299.5 ms / 332.4 KiB, 500 / 335.2 ms / 203.5 KiB y 1000 / 243.6 ms / 136.5 KiB. El scope pasa de 127 275 008 bytes sin servicios a 267 067 392 con 1000 dormidos |
| C28 | `make check && make accept` en un clon limpio, sin sudo | Sale con 0 en 2346.8 s. C8 fría sale con 77 por falta de `sudo -n` |
| C29 | cloc sobre las rutas de `acceptance/loc_paths.txt` | Producto 9347 líneas, apoyo 5095: razón 0.545, frente a la referencia de 0.15 del método |

De K6 en adelante todavía no hay mediciones.

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
