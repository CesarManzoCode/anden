# Historial de cambios

## K4: orquestación y fallos (en curso)

### Agregado

- El plano de control coloca con el scheduler de D07: filtra los nodos listos por CPU, memoria y grupo de anti-afinidad (`anti_affinity_group`), y elige por best-fit del recurso dominante, del servicio más grande al más pequeño. Dos servicios del mismo grupo no comparten nodo.
- `anden placement solve --input FILE [--json]` corre el scheduler sobre una instancia en JSON, sin datacenter, y da el nodo de cada servicio o `null` si no cabe.
- Criterio de aceptación C15 (`acceptance/c15_placement.py`), con la fuerza bruta `verify/cmd/placeopt` como comparación.

## K3: restauración, desglose por fases, descubrimiento y comparación (2026-11-07)

### Agregado

- Un servicio con `wake_route = restore` duerme con un volcado de CRIU y despierta restaurándolo. Si el volcado falla duerme como `relaunch`; si el snapshot falta o la restauración falla, despierta por `relaunch`. La instancia reporta en `route` la ruta que usó y corre sin seccomp (`seccomp = false`).
- Cada despertar escribe una línea en `nodes/nodeN/wake_events.jsonl` con la ruta usada, la pedida, la dirección del cliente que lo provocó, el instante del `accept` en `CLOCK_MONOTONIC` y siete fases en milisegundos: `detection`, `placement`, `rootfs`, `network`, `start`, `readiness` y `first_byte`. Un despertar avisa al plano de control y espera su respuesta hasta 200 ms antes de seguir en el nodo.
- Criterio de aceptación C12 (`acceptance/c12_phases.py`).
- `andend` responde DNS en `10.80.0.1:53`, por UDP y TCP: `<servicio>.anden.internal` da un registro A con TTL 1 hacia la dirección del servicio, despierto o dormido, y cualquier otro nombre da NXDOMAIN.
- `MoveService` duerme el servicio si estaba despierto, le da una dirección del nodo destino y lo deja dormido allí: el borde, el DNS y el set `sleeping` de ambos nodos siguen la dirección nueva, y el siguiente despertar ocurre en el nodo destino. `GetService` muestra el nodo y la dirección de un servicio dormido.
- Criterio de aceptación C11 (`acceptance/c11_discovery.py`).
- Una conexión que llega mientras un servicio se duerme queda retenida y se atiende. Si llega antes de que paren sus procesos (antes del volcado de CRIU en la ruta `restore`), el sueño se aborta y el servicio sigue despierto; si llega después, espera a que termine y lo despierta.
- Cada nodo escribe `nodes/nodeN/transitions.json` con el estado de cada servicio en el nodo, los sueños empezados (`sleeps`) y las conexiones retenidas mientras se dormía (`held`).
- `wakeclient measure --retry D` vuelve a conectar cada milisegundo mientras no haya conexión, y cada línea de `wakeclient` lleva `end_ns` en `CLOCK_MONOTONIC`.
- Criterios de aceptación C13 (`acceptance/c13_compare.py`) y C14 (`acceptance/c14_race.py`).

### Corregido

- La ruta `restore` despierta de verdad restaurando con CRIU. Antes, todo volcado fallaba dentro del agente y cada despertar caía a `relaunch`. Ahora los nodos de `/dev` del contenedor pasan a CRIU como montajes externos, el contenedor abre su stdio dentro de su propia raíz (anotación `anden.stdio` de `anden-runtime`) y, en esta ruta, `/dev` es un directorio del rootfs y no un `tmpfs`, con lo que una misma instancia se restaura más de una vez.
- La comparación de C13 corre sus filas de runc, crun y `criu restore`: el directorio de trabajo ya no tapa el netns del cliente, y una corrida anterior ya no impide borrarlo.

### Medido

- `<servicio>.anden.internal` sigue al servicio: tras dormirlo, moverlo de node1 a node2 y despertarlo, `dig` da la dirección nueva y la conexión funciona (C11).
- La suma de las siete fases coincide con el cliente externo dentro de máx(5 ms, 10 %) en 200 de 200 despertares, 100 por ruta (C12).
- Con n = 100 por fila, el p50 de despertar es 55.3 ms por `relaunch` y 33.6 ms por `restore`, frente a 38.0 ms de runc, 46.7 ms de crun y 16.8 ms de `criu restore` directo (C13).
- Una petición inyectada mientras el servicio se duerme se atiende en las 100 pruebas de cada ruta, sin errores ni timeouts (C14).

## K2: datacenter, API y despertar por relanzamiento (2026-11-06)

### Agregado

- API gRPC publicada en `api/anden/v1/anden.proto`, con el código Go generado en `gen/anden/v1/`: servicio `Anden` para clientes y `AgentControl` para los agentes de nodo.
- `andend`, el plano de control: responde la API en un socket Unix y en TCP, guarda el estado en PostgreSQL por socket Unix y migra el esquema al arrancar. `restore` con volúmenes se rechaza con `FAILED_PRECONDITION`.
- `anden svc create|get|list|sleep|wake|delete|deploy|move`, que envuelve la API.
- `anden up`, `down`, `status`, `exec` y `chaos kill`: levantan y destruyen el datacenter simulado (scope de systemd delegado, Postgres, namespace de usuario con el plano de control, tres nodos y un cliente), corren un comando en el netns de un nodo, del cliente o raíz, y tiran un nodo con `cgroup.kill`.
- Criterio de aceptación C27 (`acceptance/c27_lifecycle.py`): ciclo de vida sin rastro.
- `anden-agent`, el agente de nodo: se registra, late cada 500 ms y arranca y para instancias según lo que pide el plano de control. Cada instancia tiene cgroup, overlay, `resolv.conf`, namespaces de red, UTS e IPC propios, veth a `svc0` con su IP, los límites de su `ServiceSpec` y el filtro seccomp; su salida queda en `/.anden/stdio.log`. Las capas que le faltan al nodo llegan por HTTP desde `andend`, con su digest verificado.
- `andend` coloca los servicios en el primer nodo con capacidad, les asigna una IP, sirve blobs en `10.80.0.1:7444` y programa el borde: cada servicio responde en `10.80.0.2:<edge_port>`. El simulador reinicia `andend` si muere, y las instancias siguen corriendo.
- `WakeService` reemplaza una instancia en `FAILED`; `DeployService` y `MoveService` paran la instancia y arrancan otra.
- `echosvc` responde a `/hog`, `/fork` y `/burn`, que exceden a propósito sus límites.
- Criterios de aceptación C2 (`acceptance/c02_durability.py`), C4 (`c04_memory.py`) y C5 (`c05_pids_cpu.py`).
- Un servicio con `idle_seconds` mayor que 0 se duerme solo tras esa ventana sin conexiones ni tráfico saliente: sus procesos terminan, se liberan su red y su overlay, y devuelve su memoria. `SleepService` sigue el mismo camino y espera a que no queden conexiones vivas.
- Una conexión a un servicio dormido lo despierta por la ruta `relaunch` y queda retenida hasta que responde, en cualquier puerto TCP. `WakeService` sigue el mismo camino.
- Criterio de aceptación C7 (`acceptance/c07_sleep.py`).
- Los volúmenes de `ServiceSpec` se montan: cada uno es un directorio del nodo que sobrevive a dormir y despertar. Un nombre de volumen con `/`, `.` o `..` se rechaza con `INVALID_ARGUMENT`.
- `wakeclient`, cliente externo de medición: `measure`, `load` y `sample`. `apiclient` espera a que cada operación se asiente y reporta el código gRPC numérico.
- Criterios de aceptación C1 (`acceptance/c01_api.py`), C8 (`c08_warm.py` y `c08_cold.py`), C9 (`c09_no_loss.py`) y C10 (`c10_tcp.py`).

### Corregido

- Las respuestas al cliente externo vuelven por el netns raíz: cada nodo tiene una ruta `/32` hacia `10.80.0.100`. Sin ella, ninguna petición por el borde recibía respuesta.
- `memory.reclaim` se escribe sin el poller de Go. Con `EAGAIN` del kernel, `os.WriteFile` no regresaba y el servicio se quedaba en `SLEEPING`.
- El informe de C5 conserva su objeto `cpu`; el modelo de CPU va en `cpu_model`.

### Medido

- El cliente generado de la API crea, consulta, duerme, despierta y borra un servicio (C1).
- El estado de la API y los procesos de las instancias no cambian tras matar `andend` con SIGKILL (C2).
- Un servicio que excede su memoria muere dentro de su cgroup, una bomba de fork se detiene en `pids.max` y la cuota de CPU se respeta, sin errores en los demás servicios (C4, C5).
- Un servicio inactivo se queda sin procesos y devuelve su memoria (C7).
- Un despertar por `relaunch` en caché tibia responde con p99 de 63.3 ms, en 300 muestras (C8). La medición en caché fría no se hizo: falta `sudo -n`.
- Ninguna de 1600 peticiones concurrentes sobre un servicio dormido se pierde ni se duplica (C9).
- `redis-cli` sin configuración especial duerme y despierta Valkey (C10).
- `anden up` y `anden down` dejan iguales las listas de netns, cgroups, montajes y nftables (C27).

## K1: runtime OCI y almacén de imágenes (2026-11-06)

### Agregado

- `anden-runtime`, runtime OCI con `create`, `start`, `state`, `kill` y `delete` según la interfaz de línea de comandos de runtime-tools: namespaces nuevos o por ruta, rootfs con `pivot_root`, montajes, dispositivos, capacidades, rlimits, usuario, sysctl, `oomScoreAdj`, `noNewPrivileges`, rutas enmascaradas y de solo lectura, propagación del rootfs y límites de cgroups v2 (`memory.max`, `cpu.max`, `pids.max`). El estado vive en `$XDG_RUNTIME_DIR/anden-runtime/<id>/`, o en `/run/user/<uid>/anden-runtime/<id>/`, o bajo `--root`.
- `anden-runtime` aplica `linux.seccomp`: compila el perfil a BPF sin bibliotecas externas (acciones `SCMP_ACT_ALLOW`, `SCMP_ACT_ERRNO` y `SCMP_ACT_KILL`, comparación de argumentos, `errnoRet`) y lo instala antes de ejecutar el proceso del contenedor.
- Criterios de aceptación C3 (subconjunto OCI de runtime-tools, declarado en `acceptance/oci_subset.txt`) y C6 (aislamiento comparado con runc).
- `anden image import`, `unpack`, `mount` y `verify`: almacén de imágenes OCI por contenido, con sha256 verificado al importar, al transferir y al extraer, capas deduplicadas y extraídas una sola vez, y rootfs montado con overlay `userxattr`.
- Criterios de aceptación C20 (rootfs comparado con umoci), C21 (integridad) y C22 (deduplicación).

### Medido

- `anden-runtime` pasa las 20 pruebas declaradas de runtime-tools (C3) y da el mismo informe de aislamiento que runc (C6).
- El rootfs que produce `anden image` es igual al de umoci en ruta, tipo, modo y hash en alpine, nginx y una imagen sintética (C20).
- Un blob con un byte cambiado se detecta con `anden image verify` y no se puede extraer (C21).
- Las capas que comparten dos imágenes se guardan una sola vez en el almacén (C22).

## Sin publicar

### Agregado

- Contrato del sistema, con decisiones cerradas de lenguaje, arquitectura, rutas de despertar, proxy, simulador, colocación y fencing.
- Investigación con fuentes primarias y versiones de herramientas verificadas en la máquina de destino.
- Comando único de chequeo (`make check`) y CI que lo corre.

### Decisiones

- Todo el sistema corre sin root dentro de un namespace de usuario: se probó que red, nftables, overlayfs, cgroups delegados y CRIU funcionan así.
- La ruta de despertar `restore` corre sin filtro seccomp, porque CRIU sin root no puede volcar un proceso filtrado.
