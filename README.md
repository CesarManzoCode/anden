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
| K6 | Diseño y publicación de resultados | En verificación: el documento de diseño (C30) se comprueba al cerrar el K |

## Resultados

Última corrida de cada criterio: 7 nov 2026, entre las 20:40 y las 21:09 UTC, sobre el árbol `70b33e558d23`, en esta máquina (kernel 7.2.14, AMD Ryzen 5 5600G, 14 GiB visibles, btrfs, gobernador `powersave`). Las cifras se copian tal cual del informe de esa corrida. Varias son de una sola corrida o de pocas; cuánto varían entre corridas, y los demás límites, están en [`LIMITATIONS.md`](LIMITATIONS.md). El porqué de cada decisión está en [`vault/DESIGN.md`](vault/DESIGN.md).

| Criterio | Qué se mide | Cifras |
|---|---|---|
| C8 | Despertar por `relaunch` de un `echosvc` dormido, medido por un cliente externo hasta el primer byte; umbral de p99 bajo 1 s en caché tibia | Tibia, n = 300: p50 54.8 ms, p90 62.7 ms, p99 68.4 ms, máximo 141.1 ms. Bajo 1 s aquí; no se compara con Railway. Fría: **omitida** con código 77 porque `sudo -n` no está disponible |
| C13 | Cada ruta de despertar de Andén contra runc 1.5.2, crun y `criu restore` directo, n = 100 por fila | p50 / p90 / p99 / máx en ms: `anden-relaunch` 55.3 / 61.4 / 66.0 / 66.9, `anden-restore` 32.3 / 36.4 / 39.6 / 39.8, `runc` 38.0 / 39.1 / 70.7 / 70.8, `crun` 46.8 / 47.5 / 79.4 / 79.6, `criu-restore` 16.4 / 17.0 / 17.4 / 17.6. `relaunch` frente a runc y crun: comparable; `restore` frente a `criu restore`: equivalente en el mecanismo |
| C15 | Scheduler contra la fuerza bruta de `placeopt`, semillas 1 a 50, de 6 a 12 servicios | 50 instancias, 0 violaciones de capacidad o anti-afinidad. Coloca 289 servicios; brecha total de 73 frente a los 362 de la fuerza bruta, 0.798 del óptimo (289 / 362, calculado aquí), máximo 4 por instancia, óptimo en 9 de 50. Conclusión: equivalente (mismo problema y objetivo; la brecha es real) |
| C18 | Sonda externa cada 100 ms tras `anden chaos kill` de un nodo; límite de 10 s | `crash-a` 5.30 s sin servicio y `crash-b` 5.30 s, ambos de vuelta en otro nodo |
| C26 | `memory.current` del scope y p99 de 50 despertares por escalón, con `echosvc` de 50 milicores y 32 MiB por `relaunch` | Dormidos: p99 de despertar y memoria por dormido. 10: 251.8 ms y 4012.4 KiB; 50: 317.9 ms y 1072.2 KiB; 100: 249.5 ms y 639.6 KiB; 250: 299.5 ms y 332.4 KiB; 500: 335.2 ms y 203.5 KiB; 1000: 243.6 ms y 136.5 KiB. 0 errores; paró en el último escalón, no en el límite de 6 GiB |
| C29 | Líneas de código con cloc, apoyo contra producto, según `acceptance/loc_paths.txt` | Producto 9347, apoyo 5095: razón 0.545, frente a la referencia de 0.15 del método |

### Demás criterios

Misma corrida y mismo árbol. Todos pasaron; ninguno, salvo C8 fría, se omitió.

| Criterio | Qué se mide | Resultado |
|---|---|---|
| C1 | Cliente generado de la API publicada | `create`, `get`, `sleep`, `wake` y `delete` dan lo esperado, y consultar lo borrado da `NotFound` |
| C2 | Estado tras SIGKILL a `andend` | Sin diferencias, con servicios en 3 nodos |
| C3 | Subconjunto OCI de runtime-tools (`bd3b0bd`) | 20 pruebas, 2220 aserciones `ok`, 0 `not ok` |
| C4 | Servicio que excede `memory.max` | `oom_kill` 1; los otros dos servicios atienden 1441 peticiones cada uno |
| C5 | `pids.max` y cuota de CPU | `pids.events` da `max 183`; usa 0.500 CPU con cuota de 0.5 |
| C6 | `probe` en Andén y en runc | Informes idénticos; `mount` y `swapon` bloqueados |
| C7 | Sueño por inactividad | 1 proceso antes, `cgroup.procs` vacío después y `memory.current` de 499 712 bytes |
| C9 | Peticiones concurrentes a un servicio dormido | 1600 peticiones en 5 rondas, ninguna perdida ni duplicada |
| C10 | `redis-cli` contra Valkey | 15 casos en 5 ciclos de sueño |
| C11 | `dig` tras mover un servicio dormido | `10.81.0.2` en node1, luego `10.82.0.2` en node2 |
| C12 | Suma de las 7 fases contra el cliente | 100 de 100 dentro de la tolerancia en `relaunch` y 100 de 100 en `restore` |
| C14 | Petición inyectada mientras el servicio se duerme | 100 por ruta, todas atendidas; 97 cayeron dentro de la transición en cada ruta |
| C16 | Rollout con vegeta a 200 peticiones/s | 6000 peticiones, 0 errores; 50 muestras tras el overlap, todas v2 |
| C17 | Rollback de una v3 que no pasa salud | 6000 peticiones, 0 errores; v3 sin tráfico y v2 activa |
| C19 | Singleton con su nodo particionado | 2 escritores, sin solape, hueco de 2.65 s |
| C20 | Rootfs contra umoci | 3 imágenes, 5547 filas idénticas |
| C21 | Integridad de blobs | 7 blobs bien; el blob corrupto se rechaza con salida 1 |
| C22 | Deduplicación de capas | `du` 76 099 075 bytes frente a 76 072 090 de capas únicas; 7 capas compartidas |
| C23 | `promtool check metrics` | Pasa, con histogramas de las 7 fases en las 2 rutas |
| C24 | `promtool check rules` y `test rules` | Pasan, con casos que disparan y casos sanos |
| C25 | Runbooks de las alertas | 2 alertas con runbook existente |
| C27 | Rastro de `anden up` y `anden down` | Las listas de antes y después son iguales |
| C28 | `make check && make accept` en un clon limpio, sin sudo | Sale con 0 en 2346.8 s; C8 fría sale con 77 dentro del clon |

## Uso

### Requisitos

Probado solo en esta máquina, con las versiones de [`vault/RESEARCH.md`](vault/RESEARCH.md):

- Linux x86-64 con cgroups v2, namespaces de usuario sin privilegio, rangos en `/etc/subuid` y `/etc/subgid`, `newuidmap` y `newgidmap`, y una sesión de systemd de usuario que delegue `cpu`, `memory` y `pids`. No hace falta root.
- Go 1.27 y Python 3.
- En el `PATH`: `systemd-run`, `unshare` y `nsenter` (util-linux), `ip` y `tc` (iproute2), `nft` (nftables), `initdb` y `pg_ctl` (PostgreSQL 18) y, para la ruta `restore`, `criu` 4.2.1.
- Solo para `make accept`: `protoc`, `runc` y `crun` en `/usr/bin`, `umoci`, `skopeo`, `vegeta`, `promtool`, `valkey-server` y `redis-cli`, `dig`, `cloc`, `git` y `make`. `make tools` trae runtime-tools y las imágenes de `acceptance/images.lock` a `ANDEN_CACHE` (por defecto `.cache/`), así que la primera vez necesita red.

### Levantar el datacenter

```sh
make build                   # deja anden, andend, anden-agent, anden-runtime y los clientes de prueba en bin/
bin/anden up                 # plano de control, Postgres, 3 nodos y el cliente externo; estado en .run/dc
bin/anden status --json
```

Un servicio necesita una imagen OCI importada al almacén. Por ejemplo, con un layout que `skopeo copy` haya dejado en `DIR`:

```sh
bin/anden image import --layout DIR:TAG --json          # da {"digest": "sha256:..."}
bin/anden svc create --name web --image sha256:... --port 8080 \
    --cpu-millis 200 --memory-mib 64 --idle-seconds 60 --json
bin/anden svc get --name web --json                     # estado, nodo, dirección y edge_port
bin/anden exec --ns client -- curl http://10.80.0.2:EDGE_PORT/
bin/anden down                                          # destruye todo y borra el estado
```

`--wake-route restore` despierta con CRIU en vez de relanzar; `bin/anden` sin argumentos imprime todos los subcomandos.

### Verificar

```sh
make check     # formato, go vet, go test y compilación de los scripts; sin privilegios ni herramientas externas
make accept    # compila, trae herramientas y corre los criterios C1 a C30 con acceptance/run_all.py
```

`make accept` acepta las salidas 0 y 77 y falla con cualquier otra. Cada criterio levanta su propio datacenter en `.run/dc-cNN`, escribe `.run/acceptance/cNN.json` y lo destruye al salir. `ANDEN_ACCEPT_SKIP="c28 c26"` omite criterios. Sin `sudo -n`, C8 fría sale con 77. La corrida completa, medida dentro del clon de C28, tardó 2346.8 s.

## Documentos

- [`vault/CONTRACT.md`](vault/CONTRACT.md): qué debe hacer el sistema y las decisiones cerradas.
- [`vault/RESEARCH.md`](vault/RESEARCH.md): fuentes y versiones verificadas.
- [`vault/DESIGN.md`](vault/DESIGN.md): por qué el sistema es como es, sus alternativas descartadas, su deuda y lo que no se demostró.
- [`LIMITATIONS.md`](LIMITATIONS.md): lo que el proyecto no demuestra.
- [`CHANGELOG.md`](CHANGELOG.md): historial de cambios.
