# Historial de cambios

## K5: observabilidad, densidad y construcción (2026-11-07)

### Agregado

- Cada agente sirve `/metrics` en `<ip del nodo>:9090` con los histogramas `anden_wake_phase_seconds{phase, route}` y `anden_wake_duration_seconds{route}`, alimentados por cada evento de despertar. `andend` sirve en `10.80.0.1:9090/metrics` la suma de lo que reportan los agentes y `anden_node_up{node}`, que vale 0 cuando el plano de control declara muerto el nodo.
- `ops/alerts.yml` con `AndenNodeDown` y `AndenWakeOverBudget` (p99 de despertar sobre 1 s), sus pruebas en `ops/alerts_test.yml` y un runbook por alerta en `ops/runbooks/`.
- Criterios de aceptación C23 (`acceptance/c23_metrics.py`), C24 (`acceptance/c24_alerts.py`) y C25 (`acceptance/c25_runbooks.py`).
- Criterio de aceptación C26 (`acceptance/c26_density.py`): banco de densidad con 10 a 1000 servicios dormidos, que escribe `c26/curve.csv`.
- Criterios de aceptación C28 (`acceptance/c28_clean_build.py`), que corre `make check && make accept` en un clon limpio, y C29 (`acceptance/c29_ratio.py`), que compara con cloc el código de apoyo contra el de producto según las rutas de `acceptance/loc_paths.txt`.

### Cambiado

- `make check` ya no revisa el formato de los `.go` bajo `.run/`, donde C28 deja su clon.
- Un servicio que duerme sin snapshot ya no deja en su directorio el bundle, los archivos de sus namespaces ni el punto de montaje del rootfs: solo `upper/` y `work/`. El despertar por `relaunch` los vuelve a crear.

### Corregido

- Cuando el latido de un nodo se queda esperando detrás de escrituras en Postgres, el agente manda un latido aparte, que solo lee, en cuanto su ciclo normal lleva medio TTL sin renovar la concesión. Antes, con 500 servicios dormidos y el disco lento, el nodo se autofenciaba una y otra vez. El reporte de instancias de un nodo se guarda con un solo `COPY` en vez de un `INSERT` por instancia, y `andend` abre hasta 32 conexiones a Postgres en vez de una por CPU.
- `anden down` espera hasta 60 s, y no 10 s, a que el scope quede vacío después de `cgroup.kill`. Antes podía rendirse con el scope todavía vaciándose y dejar sus cgroups a la vista de la siguiente corrida.

### Medido

- `/metrics` pasa `promtool check metrics` e incluye los histogramas de las 7 fases en las 2 rutas de despertar (C23). Las reglas de alerta pasan `promtool check rules` y `test rules` (C24), y cada alerta tiene su runbook en el repositorio (C25).
- Con 10 a 1000 servicios dormidos, el p99 de despertar queda entre 243.6 y 335.2 ms, sin errores, y la memoria por servicio dormido baja de 4012.4 KiB con 10 a 136.5 KiB con 1000 (C26).
- Desde un clon limpio y sin sudo, `make check && make accept` sale con 0 (C28).
- El código de apoyo suma 5095 líneas frente a 9347 de producto, una razón de 0.545 (C29).

## K4: colocación, rollouts, caída y partición (2026-11-07)

### Agregado

- El plano de control coloca con el scheduler de D07: filtra los nodos listos por CPU, memoria y grupo de anti-afinidad (`anti_affinity_group`), y elige por best-fit del recurso dominante, del servicio más grande al más pequeño. Dos servicios del mismo grupo no comparten nodo.
- `anden placement solve --input FILE [--json]` corre el scheduler sobre una instancia en JSON, sin datacenter, y da el nodo de cada servicio o `null` si no cabe.
- Criterio de aceptación C15 (`acceptance/c15_placement.py`), con la fuerza bruta `verify/cmd/placeopt` como comparación.
- `DeployService` sobre un servicio `ACTIVE` hace rollout (D24): arranca la versión nueva junto a la vieja, con su propia dirección; cuando pasa el healthcheck del agente (HTTP o TCP) cambia el borde y el DNS en un solo paso, espera `overlap_seconds`, y el agente le envía SIGTERM a la vieja y, `draining_seconds` después, SIGKILL. Si el healthcheck no pasa en `healthcheck_timeout_seconds`, destruye la nueva sin tocar el borde y la vieja sigue activa. El despliegue vive en Postgres y sigue tras reiniciar `andend`. Sobre un servicio dormido solo cambia la versión deseada, que se aplica al despertar.
- `echosvc` responde 503 en `/healthz` si tiene `FAIL_HEALTH`.
- Criterios de aceptación C16 (`acceptance/c16_rollout.py`) y C17 (`acceptance/c17_rollback.py`), con carga de vegeta a 200 peticiones por segundo durante 30 s.
- Detección de caída y fencing (D08). El agente tiene una concesión con TTL de 2 s, contada con su reloj monótono desde el envío de cada latido aceptado. Si no la renueva, se autofencia: `cgroup.kill` de todas sus instancias, cierre de las conexiones retenidas y ningún despertar ni arranque hasta registrarse con una época nueva; entonces solo corre lo que el plano de control todavía le asigna. El plano de control declara muerto un nodo a los 4 s sin noticias y solo entonces recoloca sus servicios: lo despierto con el scheduler, del más grande al más pequeño; lo dormido se mueve sin arrancarlo y despierta por `relaunch`. Los latidos con una época vieja se rechazan, y un singleton no se activa en su nodo nuevo mientras el anterior siga vivo y reportándolo.
- `anden chaos partition nodeN`, `anden chaos heal nodeN` y `anden chaos slow nodeN --delay-ms N`: bajan y suben el enlace del nodo y le agregan retardo con netem. `heal` también quita el retardo.
- `verify/cmd/writersvc`, el singleton de prueba que escribe un latido cada 50 ms en un archivo de su volumen.
- Criterios de aceptación C18 (`acceptance/c18_node_crash.py`) y C19 (`acceptance/c19_partition.py`).

### Cambiado

- `DeployService` ya no para la instancia vieja antes de arrancar la nueva, salvo en un singleton. Mientras haya un despliegue en curso, otro sobre el mismo servicio responde `FAILED_PRECONDITION`.
- `GetService`, `ListServices` y `anden status` ya no muestran instancias de un nodo muerto.

### Medido

- En 50 instancias de 6 a 12 servicios, el scheduler nunca excede la capacidad de un nodo ni junta dos servicios del mismo grupo de anti-afinidad. Coloca 289 servicios frente a 362 de la fuerza bruta, y alcanza el óptimo en 9 instancias (C15).
- Un rollout bajo 200 peticiones por segundo no pierde ninguna de 6000, y tras el overlap solo responde v2 (C16).
- Una versión cuyo healthcheck falla arranca pero no recibe ninguna petición, y la anterior sigue activa sin errores en 6000 peticiones (C17).
- Al matar un nodo, sus dos servicios responden desde otro nodo tras 4.70 s sin servicio (C18).
- Con el nodo de un singleton particionado, el escritor viejo para antes de que arranque el nuevo, con 2.645 s entre ambos (C19).

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
