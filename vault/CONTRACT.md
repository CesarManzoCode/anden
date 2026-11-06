# Contrato de Andén

Fuente de verdad del sistema. Etiquetas: **[observado]** medido en esta máquina el 6 nov 2026 (fuentes en `vault/RESEARCH.md`), **[diseño]** decisión aceptada, **[hipótesis]** sin medir todavía. Nada aquí es comportamiento implementado hasta que un criterio lo demuestre.

## 1. Qué debe hacer el sistema

Andén es una plataforma de contenedores con scale-to-zero sobre un datacenter de 3 nodos simulados en una sola máquina. Su eje de diseño es el **presupuesto de arranque en frío**: el tiempo desde la primera petición a un servicio dormido hasta su primer byte de respuesta.

Debe, medido siempre por un cliente externo y contra una comparación externa:

1. Exponer una API con estado deseado durable que gestione servicios sobre 3 nodos.
2. Ejecutar contenedores con un runtime propio (namespaces, cgroups v2, seccomp, overlayfs), conforme a un subconjunto declarado del runtime OCI.
3. Dormir cualquier servicio TCP inactivo y despertarlo reteniendo la primera conexión, sin perder ni duplicar peticiones.
4. Comparar dos rutas de despertar entre sí y contra runc, crun y `criu restore`, con desglose por fases.
5. Resolver servicios por nombre interno aunque despierten o se muevan de nodo.
6. Colocar con capacidad y anti-afinidad, desplegar sin downtime, revertir y recolocar ante caída o partición, sin dos instancias activas de un singleton.
7. Almacenar imágenes OCI por contenido, con capas deduplicadas y rootfs equivalente al de una herramienta de referencia.
8. Exponer métricas Prometheus, alertas probadas y runbooks.
9. Levantar y destruir todo con un comando, sin dejar rastro.

Los criterios C1–C30 de la idea son las pruebas de todo esto. La sección 6 fija el script, el informe y el K de cada uno.

## 2. Hechos de la máquina que condicionan el diseño

- **[observado]** x86-64, Ryzen 5 5600G (12 hilos), 14 GiB visibles, kernel 7.2.14, Arch Linux, `/home` en btrfs, gobernador `powersave`, `/dev/kvm` accesible.
- **[observado]** `sudo` pide contraseña. `/etc/subuid` y `/etc/subgid` dan 65536 ids, y existen `newuidmap` y `newgidmap`.
- **[observado]** `unshare -U --map-root-user --map-auto` funciona. Dentro funcionan bridge, veth, nftables con NAT, `redirect`, `SO_ORIGINAL_DST`, `tc netem`, `ip_forward` y overlayfs sobre btrfs con `userxattr`, incluidos whiteouts `mknod c 0 0` y `user.overlay.opaque`.
- **[observado]** `systemd-run --user --scope -p Delegate=yes` entrega `cpu memory pids` con escritura real de `cpu.max`, `memory.max`, `pids.max`, `cgroup.kill`, `memory.reclaim`. El proceso debe salir de la raíz del scope antes de activar `cgroup.subtree_control`.
- **[observado]** CRIU 4.2.1 hace dump y restore de un contenedor con PID, red, montaje, UTS e IPC propios sobre overlay, sin root: restore de 12 ms, dump de unos 95 ms y estado preservado (un contador en memoria siguió en 3). Requisitos en D03.
- **[observado]** Sin root, CRIU no puede volcar un proceso con filtro seccomp: `PTRACE_O_SUSPEND_SECCOMP` falla con EPERM (`suspending seccomp failed`).
- **[observado]** La suite runtime-tools, en ese mismo entorno, corre contra crun: 20 pruebas pasan limpias. Son el subconjunto de D09.
- **[observado]** `runc create` funciona dentro del namespace y el servicio responde, pero `runc start` y `runc run -d` (runc 1.5.1) no regresan. crun regresa bien. Ver D20.

## 3. Arquitectura

### Binarios

| Binario | Papel | Clase para C29 |
|---|---|---|
| `andend` | Plano de control: API gRPC, reconciliador, detector de fallos, scheduler, DNS, servidor de blobs, programación del borde | producto |
| `anden-agent` | Agente de nodo: instancias, proxy de despertar, detección de inactividad, autofencing | producto |
| `anden-runtime` | Runtime OCI (`state`, `create`, `start`, `kill`, `delete`) | producto |
| `anden` | CLI y subcomandos de simulador (`up`, `down`, `exec`, `chaos`) | producto; lo del simulador en `internal/sim` es apoyo |

### Rutas del repositorio

| Ruta | Contenido |
|---|---|
| `api/anden/v1/anden.proto` | Definición publicada de la API |
| `gen/anden/v1/` | Código generado, versionado, excluido del conteo de C29 |
| `cmd/`, `internal/` | Producto |
| `internal/sim/` | Simulador del datacenter (apoyo) |
| `verify/cmd/` | Clientes y servicios de prueba externos: `probe`, `echosvc`, `writersvc`, `wakeclient`, `apiclient`, `mkimage`, `placeopt` (apoyo). Nunca importan `internal/` |
| `acceptance/` | `lib.py`, `fetch_tools.py`, `run_all.py`, `images.lock`, `oci_subset.txt`, `loc_paths.txt` y `cNN_<slug>.py` (apoyo) |
| `ops/` | `alerts.yml`, `alerts_test.yml`, `runbooks/*.md` |
| `vault/DESIGN.md` | Documento de diseño de C30 |
| `.run/` y `.cache/` | Estado y caché locales, ignorados por git |

No se crean subcarpetas de `vault/`: el contrato y la investigación bastan.

### Topología

```
scope systemd --user anden-<h>.scope (Delegate=yes)          h = sha1(ruta absoluta del estado)[:12]
├─ sup/        anden _dc (uid del usuario)
├─ pg/         postgres (uid del usuario, sin TCP, socket Unix)
└─ dc/         anden _dc-init: PID 1 de un namespace user+pid+net+mnt
   ├─ cp/      andend, en el netns raíz (10.80.0.1)
   ├─ hold/    procesos que sostienen los netns client y nodeN
   └─ nodeN/   agent/ y svc-<servicio>-<generación>/, un cgroup por instancia
```

- Red raíz del datacenter: bridge `anden0`, `10.80.0.1/24`, más la dirección de borde `10.80.0.2/32`.
- Nodos: netns propios, `10.80.0.11`, `.12`, `.13`, conectados con veth a `anden0`. La veth del lado raíz se llama `veth-nodeN`.
- Subred de servicios de cada nodo: `10.81.0.0/16`, `10.82.0.0/16`, `10.83.0.0/16`, sobre el bridge `svc0` del nodo con gateway `.0.1`. El netns raíz tiene una ruta por nodo.
- Cliente externo: netns `client`, `10.80.0.100`, gateway `10.80.0.1`.
- Cada instancia tiene netns propio con veth a `svc0` y una IP fija asignada por el plano de control.

### Estado en disco (`--state`, por defecto `.run/dc`)

| Ruta | Contenido |
|---|---|
| `dc.json` | `{"scope_cgroup": "/sys/fs/cgroup/...", "dc_pid": N, "ns": {"root": pid, "client": pid, "node1": pid, "node2": pid, "node3": pid}, "pg_socket_dir": "...", "api_socket": "...", "edge_ip": "10.80.0.2"}` |
| `pg/` | Datos de Postgres |
| `registry/` | Almacén central de imágenes |
| `nodes/nodeN/store/` | Almacén local del nodo (`blobs/sha256/<hex>`, `layers/<hex>/`) |
| `nodes/nodeN/instances/<servicio>-<gen>/` | `upper/`, `work/`, `merged/`, `bundle/config.json`, `net.ns`, `uts.ns`, `ipc.ns` |
| `nodes/nodeN/snapshots/<servicio>-<gen>/` | Imágenes de CRIU |
| `nodes/nodeN/wake_events.jsonl` | Una línea por despertar (sección 5) |

Los sockets Unix viven en `$XDG_RUNTIME_DIR/anden-<h>/`, o `/tmp/anden-<h>/` si falta esa variable, porque una ruta dentro de un clon anidado supera el límite de 107 bytes de `sun_path`.

## 4. Decisiones cerradas

### Delegadas por la idea

**D01. Lenguajes.**
Decisión: producto en Go 1.27 con `CGO_ENABLED=0`; verificadores en Python 3 con solo biblioteca estándar (`acceptance/`) y clientes de medición en Go (`verify/cmd/`).
Razón: Go es lo que pide la vacante y permite el patrón de reejecución para namespaces sin C; Python mantiene pequeños los verificadores y no agrega dependencias.

**D02. Plano de control, agente y estado.**
Decisión: un `andend` y un `anden-agent` por nodo, unidos por gRPC sobre la red simulada; estado deseado y observado en PostgreSQL 18 por socket Unix, con pgx v5; un reconciliador de nivel (compara deseado contra observado) sin colas externas.
Razón: es la pila Go, gRPC y Postgres de la vacante; el agente habla por la red simulada para que una partición lo corte de verdad; Postgres, fuera del árbol de nodos, da durabilidad real ante SIGKILL del plano de control.

**D03. Rutas de despertar.**
Decisión: dos rutas por servicio con el campo `wake_route`: `relaunch` (por defecto), que crea netns y arranca el proceso, y `restore`, que restaura con CRIU; si el dump o el restore fallan, cae a `relaunch` y registra la ruta usada.
Razón: `relaunch` funciona con todo servicio; `restore` conserva memoria y es la comparación pedida; la caída evita que un snapshot perdido o inválido deje el servicio sin despertar. Se descarta el freezer de cgroups porque deja procesos vivos y viola C7.

Receta de `restore` **[observado]**, que el agente sigue tal cual:

- El contenedor hace `setsid`, no usa `PR_SET_PDEATHSIG` y su stdio es el archivo `/.anden/stdio.log` dentro del rootfs. La cgroup, el netns y los namespaces UTS e IPC los prepara el agente antes de arrancarlo.
- Dump, en un namespace de montaje privado y con el proceso ya dentro de los UTS e IPC del contenedor: `mount --bind M M`, `tmpfs` sobre `M/tmp`, `proc` sobre `M/proc`, bind de `/dev/null` sobre `M/dev/null`, bind del directorio de snapshot sobre `M/tmp/img`, copia de `criu` y sus bibliotecas (`ldd`) a `M/tmp/c/`, `chroot M`, y entonces `/tmp/c/ld-linux-x86-64.so.2 --library-path /tmp/c /tmp/c/criu dump --network-lock skip --unprivileged --manage-cgroups=ignore -t PID -D /tmp/img --root / --external net[INODO]:extnet -o dump.log -v2`. Esta vista es necesaria porque, sin root, CRIU resuelve los archivos mapeados por ruta dentro de su propio namespace de montaje y exige el mismo inodo.
- Restore, con el proceso `criu` ya dentro de la cgroup de la instancia y de los UTS e IPC retenidos, tras montar overlay y `mount --bind M M`: `criu restore --network-lock skip --unprivileged --manage-cgroups=ignore -D IMG --root M --inherit-fd fd[N]:extnet -d --pidfile F -o restore.log -v2`, con N abierto sobre el netns preparado.
- Los servicios Go deben desactivar MPTCP porque CRIU rechaza esos sockets: `echosvc` y `writersvc` compilan con la directiva `//go:debug multipathtcp=0`.
- La ruta `restore` no admite volúmenes (la API responde `FAILED_PRECONDITION`) ni filtro seccomp: la instancia corre sin él y la API lo reporta en `instance.seccomp = false` (ver D19).
- Tras el dump, el agente escribe en `memory.reclaim` para devolver la memoria de la cgroup.

**D04. Proxy de despertar.**
Decisión: proxy L4 en espacio de usuario con redirección del kernel. Cada nodo tiene `table inet anden_wake` con el set `sleeping` (`ipv4_addr . inet_service`) y una regla `ip daddr . tcp dport @sleeping redirect to :15000` en `prerouting` y `output`. El agente acepta en 15000, lee `SO_ORIGINAL_DST`, retiene la conexión, despierta, sondea TCP cada 2 ms hasta 10 s, y copia bytes en ambos sentidos con `io.Copy` sobre `*net.TCPConn` (usa `splice`). Al terminar el despertar saca el destino del set en una transacción `nft -f -`: las conexiones nuevas van directo; las establecidas siguen por el proxy hasta cerrarse porque conntrack conserva su redirección.
Razón: es el modelo público de Railway, sirve para cualquier TCP y nunca rompe una conexión viva; el sondeo de 2 ms baja el piso de latencia frente a los 30 ms que Railway describe.

**D05. Señal de inactividad.**
Decisión: un servicio está inactivo si en toda la ventana no tuvo sockets TCP `ESTABLISHED` ni `SYN_RECV` (leídos de `/proc/<pid>/net/tcp` y `tcp6`) y no creció `tx_packets` de su veth. La ventana es `idle_seconds`: 60 por defecto, 2 en pruebas, 0 significa no dormir nunca.
Razón: combina las dos señales conocidas: una conexión abierta inactiva no se corta (Fly.io) y el tráfico saliente cuenta como actividad (Railway).

**D06. Simulador de nodos.**
Decisión: nodos como árboles de procesos con netns y cgroup propios dentro de un único namespace de usuario, sin VMs; caída con `cgroup.kill` del cgroup del nodo; partición con `ip link set veth-nodeN down` en el netns raíz; lentitud con `tc qdisc` `netem` en esa misma veth.
Razón: **[observado]** todo funciona sin root y gasta muy poca memoria; una VM por nodo restaría RAM al banco de densidad y no aporta a C18 ni C19, porque el fallo se inyecta igual.

**D07. Colocación.**
Decisión: filtro (CPU y memoria libres, anti-afinidad) y luego best-fit por recurso dominante, procesando en orden decreciente de tamaño en lote. Reserva solo lo despierto; lo dormido reserva nada. Un despertar va al nodo hogar si cabe; si no, el scheduler elige otro y mueve la dirección. Comparación externa: `verify/cmd/placeopt`, fuerza bruta sobre 3^n para n ≤ 12. Objetivo: maximizar servicios colocados; en empate, minimizar la mayor ocupación de nodo.
Razón: best-fit concentra y deja huecos grandes para los servicios de mayor tamaño; la fuerza bruta es exacta en este tamaño y no depende de un solver instalado.

**D08. Detección de caída y fencing.**
Decisión: el agente renueva una concesión cada 500 ms con TTL de 2 s medido con su reloj monótono; si no renueva dentro del TTL, se autofencia: `cgroup.kill` de todas sus instancias y cierre de las conexiones retenidas, sin volver a despertar nada hasta registrarse con una nueva época. El plano de control declara el nodo muerto a los 4 s (2×TTL) y solo entonces recoloca. Caída y partición se tratan igual porque no se pueden distinguir desde fuera. Lo dormido se recoloca moviendo su dirección; su snapshot se da por perdido y despierta con `relaunch`.
Razón: como el fencing termina a los 2 s y la recolocación empieza a los 4 s, dos escritores no pueden solaparse mientras el reloj monótono avance igual para ambos, cosa cierta en una sola máquina y documentada como límite para una real.

**D09. Subconjunto OCI.**
Decisión: `acceptance/oci_subset.txt` declara exactamente estas 20 pruebas de runtime-tools (commit en RESEARCH): `config_updates_without_affect create default hostname kill kill_no_effect killsig linux_ns_nopath linux_ns_path linux_ns_path_type linux_rootfs_propagation linux_seccomp linux_sysctl process process_capabilities process_oom_score_adj process_user root_readonly_true start state`. Se corren dentro de `systemd-run --user --scope -p Delegate=yes` y `unshare -U --map-root-user --map-auto --net --mount --pid --fork --kill-child --mount-proc`, con `RUNTIME=<ruta de anden-runtime>`, desde el directorio de runtime-tools.
Razón: **[observado]** son las que crun pasa limpias en ese entorno, así que el piso lo fija un runtime de referencia. Quedan fuera hooks, los límites de cgroup de la suite (que suponen cgroup raíz), AppArmor, etiquetas de montaje y `linux_uid_mappings`.

**D10. Herramienta de referencia y formato del almacén.**
Decisión: `umoci unpack --rootless --image <layout>:<tag> <bundle>` es la referencia de C20. El almacén usa el formato de OCI layout (`blobs/sha256/<hex>`) más `layers/<hex-del-diffid>/` con cada capa extraída una sola vez; los whiteouts se traducen a char device 0/0 y los opacos a `user.overlay.opaque`, y el rootfs se monta con overlay `userxattr`. El digest se verifica al importar, al transferir a un nodo y al extraer. Un rootfs ya extraído no se revierifica en cada despertar; `anden image verify` recorre todo.
Razón: **[observado]** umoci sin root produce `rootfs/`; verificar en cada despertar costaría presupuesto a cambio de nada que C21 pida. C20 compara ruta, tipo, modo y hash, no dueño, porque un rootfs sin root no puede preservarlo igual.

**D11. Generador de carga y cliente de medición.**
Decisión: `vegeta attack -rate=200/s -duration=30s -keepalive=false` con `vegeta report -type=json` para C16 y C17; `verify/cmd/wakeclient` (Go, usa `net.Dialer` y `time.Now`, reloj monótono) mide desde antes de `connect()` hasta el último byte, y da IDs únicos en C9 y C14.
Razón: vegeta es de lazo abierto, así que no reduce carga si el sistema se frena; `-keepalive=false` obliga a cada petición a seguir la tabla del borde; un cliente propio hace falta para cruzar IDs y puertos con el desglose.

**D12. Umbrales numéricos.**
Decisión: C5, consumo de CPU entre 0.50 y 1.10 × cuota, con `usage_usec` en 5 s; C7, `idle_seconds = 2` y `memory.current` ≤ 8 MiB tras `memory.reclaim`; C8, n = 300 tras descartar 30 de calentamiento, p99 < 1000 ms en tibia, y n = 50 sin umbral en fría; C9, 32 clientes, 5 rondas, 10 peticiones por cliente y ronda; C12, al menos 99 % de los despertares con |suma de fases − cliente| ≤ máx(5 ms, 10 % del cliente), con n ≥ 100 por ruta; C13, n = 100 por fila; C14, M = 100 por ruta; C15, semillas 1 a 50 con n de 6 a 12; C16 y C17, 200 peticiones por segundo durante 30 s, overlap 5 s y draining 2 s; C18, 10 s sin servicio como máximo; C26, escalones 10, 50, 100, 250, 500 y 1000 servicios dormidos, parando al alcanzar 6 GiB de `memory.current` del scope; defaults del producto, `overlap_seconds = 20`, `draining_seconds = 20`, `healthcheck_timeout_seconds = 60`.
Razón: C5 y C12 toleran el ruido del planificador sin ocultar un límite roto; C18 es detección de 4 s más margen para varios arranques; 300 muestras dan un p99 con más de una observación por encima; 6 GiB dejan margen en 14 GiB; los defaults son propios porque la página de Railway consultada no publica los suyos. Son decisiones de diseño, no mediciones: un umbral solo cambia con su razón escrita aquí.

**D13. Fila de Firecracker.**
Decisión: no se agrega.
Razón: exige kernel de invitado y rootfs ext4 aparte, y mide una garantía distinta; la idea lo permite solo como comparación y C13 no lo pide.

**D14. Orden de fases.**
Decisión: seis K: K1 runtime e imágenes; K2 datacenter, API y despertar por relanzamiento; K3 restauración, desglose, descubrimiento y comparación; K4 orquestación y fallos; K5 observabilidad, escala y construcción; K6 diseño y publicación de resultados.
Razón: se entrega primero lo que sostiene el presupuesto de arranque. Como los mini sprints no verifican y un cierre solo admite un fixer, K cortos limitan cuánto código sin probar se acumula; K6 existe porque los resultados solo se pueden publicar después de medir.

**D15. Confirmaciones en la máquina.**
Decisión: confirmados x86-64, namespaces de usuario sin privilegio, overlayfs sin root sobre btrfs y delegación de cgroups v2 (sección 2).
Razón: se ejecutaron aquí; dejan de ser hipótesis.

**D16. Huecos de investigación.**
Decisión: se cierran como *sin cifra oficial*: la documentación de snapshots de Firecracker no publica latencia de restauración; la de gVisor no da cifra en texto; la única cifra pública de Railway es < 1 s, de septiembre de 2023; no se encontró si Railway usa snapshots o SOCKMAP en producción. Ningún umbral depende de ellas salvo C8, que usa esa cifra pública.
Razón: se consultaron las fuentes primarias y no hay dato. No se usan cifras de blogs.

### Abiertas por el diseño

**D17. Modelo de permisos y código 77.**
Decisión: todo corre sin root dentro del namespace del datacenter. Un criterio marcado «requiere sudo» en la idea se ejecuta de verdad si los permisos reales que necesita existen, y sale con 77 solo si faltan. Solo C8 en caché fría necesita root real. Los checkers aceptan un 77 solo tras comprobar por su cuenta que el permiso falta; si está y el script dio 77, es FAIL.
Razón: exigir sudo donde no hace falta dejaría sin demostrar C3–C6, C13, C18, C19 y C27 en una corrida sin contraseña. La idea pide el 77 cuando falta el permiso, no cuando se pudo evitar.

**D18. Estados de caché.**
Decisión: *tibia* es capas presentes en el nodo sin vaciar nada; *fría* es tras `sync` y escribir 3 en `/proc/sys/vm/drop_caches` con `sudo -n`.
Razón: resuelve una contradicción: la investigación llama tibia al estado con page cache vaciada y la idea exige sudo solo en fría. Gana la idea.

**D19. Seccomp y restauración.**
Decisión: `relaunch` aplica el filtro seccomp; `restore` no, y lo reporta en la API y en `LIMITATIONS.md`.
Razón: **[observado]** CRIU sin root no puede volcar un proceso filtrado, y la capacidad que necesita exige `CAP_SYS_ADMIN` en el namespace inicial.

**D20. runc en las comparaciones.**
Decisión: C6 y C13 usan `/usr/bin/runc` y `/usr/bin/crun` con `create`, y lanzan `start` sin esperar su retorno (se mata a los 2 s); el tiempo lo mide `wakeclient`.
Razón: **[observado]** `runc start` no regresa en este entorno aunque el contenedor sí arranca. La medida es externa, así que no depende de ese retorno.

**D21. Borde.**
Decisión: `table ip anden_edge` en el netns raíz con el mapa `svc` (`inet_service : ipv4_addr . inet_service`) y `dnat ip to tcp dport map @svc` para `10.80.0.2`, en `prerouting` y `output`. Cada servicio recibe un puerto de borde desde 30000. Los cambios se aplican en una transacción `nft -f -` con `delete element` y `add element`.
Razón: da un punto estable a los generadores externos durante rollouts, y conntrack mantiene las conexiones vivas en la versión anterior hasta el draining.

**D22. Descubrimiento.**
Decisión: DNS propio en `andend` (`miekg/dns`), `10.80.0.1:53`, `<servicio>.anden.internal`, registro A con TTL 1 hacia la IP de la instancia o, si duerme, hacia su IP reservada. Dormido en un nodo, la dirección apunta al set `sleeping` de ese nodo; mover un servicio dormido reasigna su IP al nodo destino.
Razón: el nombre de un servicio dormido debe despertarlo; TTL 1 acota el tiempo con una dirección vieja.

**D23. API.**
Decisión: gRPC definido en `api/anden/v1/anden.proto`, servicio público `Anden` con `CreateService`, `GetService`, `ListServices`, `SleepService`, `WakeService`, `DeleteService`, `DeployService`, `MoveService`, `ListNodes`; servicio interno `AgentControl` para agentes. Estados del enum: `SERVICE_STATE_CREATING`, `SERVICE_STATE_ACTIVE`, `SERVICE_STATE_SLEEPING`, `SERVICE_STATE_ASLEEP`, `SERVICE_STATE_WAKING`, `SERVICE_STATE_FAILED`. El proto declara `option go_package = "github.com/CesarManzoCode/anden/gen/anden/v1;andenv1"`. Escucha en el socket Unix de `dc.json` para clientes del host y en `10.80.0.1:7443` para agentes.
Razón: es lo que pide la vacante; el socket Unix deja al CLI hablar desde fuera del namespace.

**D24. Rollout y rollback.**
Decisión: `DeployService` arranca la versión nueva junto a la vieja; al pasar el healthcheck cambia el borde y el DNS, espera `overlap_seconds`, envía SIGTERM, espera `draining_seconds` y envía SIGKILL. Si el healthcheck no pasa en `healthcheck_timeout_seconds`, destruye la nueva sin tocar el borde y deja activa la anterior. Un despliegue sobre un servicio dormido solo cambia la versión deseada: se aplica al despertar.
Razón: es la semántica de overlap y draining que Railway documenta; un servicio dormido no tiene tráfico que proteger.

**D25. Imágenes de prueba.**
Decisión: C20 y C22 usan imágenes reales fijadas por digest en `acceptance/images.lock` (digests en RESEARCH) y traídas con `skopeo copy` a `.cache/images/`, más una imagen sintética de `verify/cmd/mkimage` con whiteouts y directorio opaco. C10 empaqueta `valkey-server` y sus bibliotecas de la distro en una imagen; el cliente es `redis-cli`.
Razón: `nginx:1-trixie` y `nginx:1-trixie-perl` comparten 7 capas **[observado]**; Arch provee `redis-cli` como enlace a `valkey-cli`.

**D26. Licencia y visibilidad.**
Decisión: MIT.
Razón: el propósito es servir de evidencia pública en una solicitud; el método pide LICENSE si el repositorio es público.

**D27. Presupuesto de verificación.**
Decisión: C29 compara según la idea: apoyo (`acceptance/`, `verify/`, `internal/sim/`) contra producto (`cmd/`, `internal/` sin `internal/sim/`), sin `*_test.go` ni `gen/` en ninguno. Se reporta además la razón frente a la referencia del 15 % del método.
Razón: contradicción resuelta: el umbral del producto lo fija la idea (menor que el producto); el 15 % del método es referencia, no límite. Los checkers privados no cuentan porque no son parte del repositorio.

**D28. Evidencia.**
Decisión: cada checker añade una sección fechada a `vault/private/evidence/kN.md`, con los números crudos de cada criterio y la lista de omisiones por 77. No lo escribe ningún mini sprint.
Razón: el método pide evidencia por K y prohíbe verificar dentro de los mini sprints.

## 5. Interfaces

### CLI `anden`

Todo subcomando acepta `--state DIR`. `--json` imprime un objeto JSON en stdout.

| Comando | Efecto |
|---|---|
| `anden up` | Levanta el datacenter desde cero; regresa cuando `dc.json` existe y los 3 nodos están registrados |
| `anden down` | `cgroup.kill` del scope, espera a que se vacíe, borra el estado (dentro del mapeo de ids) |
| `anden status --json` | `{"nodes": [{"name", "state", "epoch"}], "services": [...]}` |
| `anden exec --ns client\|root\|nodeN -- CMD...` | Corre CMD en ese namespace de red, dentro del namespace de usuario, con `nsenter --preserve-credentials -U -n -t <pid>` |
| `anden chaos kill\|partition\|heal nodeN` | Inyecta el fallo de D06 |
| `anden chaos slow nodeN --delay-ms N` | Agrega retardo con netem |
| `anden node restart nodeN` | Reinicia el agente tras una caída |
| `anden image import --layout DIR:TAG --json` | Importa al `registry/`; devuelve `{"digest": ...}` |
| `anden image unpack --digest D --dest DIR` | Extrae y verifica; sale ≠ 0 si un blob no coincide |
| `anden image mount --digest D --dest DIR` | Corre dentro del mapeo de ids y monta el overlay |
| `anden image verify` | Verifica todos los blobs |
| `anden svc create\|get\|list\|sleep\|wake\|delete\|deploy\|move ... --json` | Envuelve la API |
| `anden placement solve --input FILE --json` | Corre el scheduler sobre una instancia, sin datacenter |

### Eventos de despertar

`wake_events.jsonl`, una línea por despertar:

```json
{"service": "s", "route": "relaunch", "route_requested": "restore", "client_addr": "10.80.0.100:40112", "t_accept_ns": 0, "phases_ms": {"detection": 0.0, "placement": 0.0, "rootfs": 0.0, "network": 0.0, "start": 0.0, "readiness": 0.0, "first_byte": 0.0}}
```

### Métricas

`andend` y cada agente exponen `/metrics`. El agregado en `10.80.0.1:9090/metrics` incluye `anden_wake_phase_seconds` (histograma, etiquetas `phase` con las 7 fases y `route`) y `anden_wake_duration_seconds` (histograma, etiqueta `route`).

### Alertas

`ops/alerts.yml` define al menos `AndenNodeDown` y `AndenWakeOverBudget`, cada una con la anotación `runbook` hacia una ruta del repositorio bajo `ops/runbooks/`.

## 6. Contrato de aceptación

Cada criterio es un script `acceptance/cNN_<slug>.py` que escribe `.run/acceptance/cNN.json`, deja sus archivos crudos en `.run/acceptance/cNN/` y sale con 0, 1 o 77. La única excepción es C8: `c08_warm.py` escribe `c08.json` y `c08_cold.py` escribe `c08cold.json`. Todo informe lleva `"criterion": "CNN"`, `"tree"` (sello calculado por `lib.tree_stamp()`), `"kernel"` y `"cpu"`. Con 77 escribe `"skipped": "<motivo>"` y lo imprime.

`lib.tree_stamp()` es el sha256 de la salida concatenada de `git rev-parse HEAD`, `git status --porcelain` y `git diff HEAD`. Los checkers reutilizan un informe solo si su sello coincide con el árbol actual; si no, corren el script. Cada script levanta su propio datacenter con `--state .run/dc-cNN` y lo destruye al salir.

`make accept` corre todo con `acceptance/run_all.py`, acepta 0 y 77 y falla en cualquier otro código. Omite los criterios listados en `ANDEN_ACCEPT_SKIP`, separados por espacios. Usa `ANDEN_CACHE` como caché de imágenes y runtime-tools.

`make check` no levanta el datacenter: `go test ./...` no requiere privilegios ni herramientas externas, y una prueba que los necesite usa `t.Skip`. Corre igual en CI.

Campos obligatorios del informe, además de los comunes:

| C | K | Script | Campos y archivos crudos |
|---|---|---|---|
| C1 | k2 | `c01_api.py` | `generated_diff_empty` (bool: regenerar con `make gen` a un directorio temporal no cambia `gen/`), `ops`: lista de `{"op", "grpc_code", "state"}` con `create`, `get`, `sleep`, `wake`, `delete`, `get_after_delete`; `state` usa los nombres del proto; crudo `c01/client.log` |
| C2 | k2 | `c02_durability.py` | `signal: "SIGKILL"`, `pid_before`, `pid_after`, `nodes_with_services`; crudos `c02/before.json`, `c02/after.json` |
| C3 | k1 | `c03_oci.py` | `results`: `{prueba: {"rc": int}}`; crudos `c03/<prueba>.tap` |
| C4 | k2 | `c04_memory.py` | `memory_events_raw` (contenido de `memory.events`), `other_services`: lista de `{"name", "requests", "errors"}` |
| C5 | k2 | `c05_pids_cpu.py` | `pids`: `{"pids_max", "pids_events_raw", "pids_current", "host_fork_ok", "others": [{"name", "requests", "errors"}]}`; `cpu`: `{"quota_fraction", "usage_usec_start", "usage_usec_end", "wall_seconds"}` |
| C6 | k1 | `c06_isolation.py` | crudos `c06/anden.json`, `c06/runc.json`, ambos del binario `probe`: `{"pid1": bool, "hostname_own": bool, "host_procs_visible": bool, "blocked_syscalls": {nombre: "EPERM"}}` |
| C7 | k2 | `c07_sleep.py` | `idle_seconds`, `waited_seconds`, `procs_before` (int), `cgroup_procs_raw`, `memory_current_bytes`, `state` |
| C8 | k2 | `c08_warm.py` | `cache: "warm"`, `samples_ms`, `warmup_discarded`, `errors`, `p50`, `p90`, `p99`, `max`, `governor`, `disk` |
| C8 fría | k2 | `c08_cold.py` | `cache: "cold"`, `samples_ms`, `errors`, `p50`, `p90`, `p99`, `max`; 77 si `sudo -n` falla |
| C9 | k2 | `c09_no_loss.py` | `clients`, `rounds`, `rounds_asleep_verified`; crudos `c09/client.log` y `c09/service.log`: una línea JSON por petición, `{"id", "ok"}` y `{"id"}` |
| C10 | k2 | `c10_tcp.py` | `cycles`, `slept_verified`, `client_args` (lista), `cases`: lista de `{"cmd", "expected", "actual"}` |
| C11 | k3 | `c11_discovery.py` | `name`, `node_before`, `node_after`, `addr_before`, `addr_after`, `dig_raw_before`, `dig_raw_after`, `connect_after_ok` |
| C12 | k3 | `c12_phases.py` | `wakes`: lista de `{"route", "client_ms", "phases_ms"}` con las 7 fases |
| C13 | k3 | `c13_compare.py` | `image`, `rows`: lista de `{"route", "p50", "p90", "p99", "max", "n", "conclusion"}`; `route` en `anden-relaunch`, `anden-restore`, `runc`, `crun`, `criu-restore`; `conclusion` en `equivalente`, `comparable`, `distinto` |
| C14 | k3 | `c14_race.py` | `routes`: `{ruta: {"m", "errors", "timeouts", "hits_during_transition"}}` |
| C15 | k4 | `c15_placement.py` | crudos `c15/instance-<semilla>.json`: `{"seed", "problem": {"nodes": [{"name", "cpu_millis", "mem_mib"}], "services": [{"name", "cpu_millis", "mem_mib", "anti_affinity_group"}]}, "placement": {servicio: nodo o null}, "optimal_placed": int}` |
| C16 | k4 | `c16_rollout.py` | `after_overlap_expected: "v2"`; crudos `c16/vegeta.json`, `c16/after.log` (una versión por línea) |
| C17 | k4 | `c17_rollback.py` | `final_active_version`; crudos `c17/vegeta.json`, `c17/v3_requests.log` |
| C18 | k4 | `c18_node_crash.py` | `killed_node`, `limit_seconds`, `services`: lista de `{"name", "node_before", "node_after", "downtime_seconds"}` |
| C19 | k4 | `c19_partition.py` | `partition_start_ns`, `heal_ns`, `node_partitioned`; crudo `c19/writers.log`: `{"writer", "t_ns"}` por latido |
| C20 | k1 | `c20_rootfs.py` | `images` (lista de nombres); crudos `c20/<imagen>.anden.tsv` y `.umoci.tsv`: `ruta\ttipo\tmodo\tsha256`, ordenados |
| C21 | k1 | `c21_integrity.py` | `blobs_checked`, `mismatches`, `corrupted_blob`, `corrupted_use_rc`, `corrupted_use_stderr` |
| C22 | k1 | `c22_dedup.py` | `du_bytes`, `unique_layers_bytes`, `shared_layers` (digests), `copies`: `{digest: número de archivos en el almacén}` |
| C23 | k5 | `c23_metrics.py` | `promtool_rc`; crudo `c23/metrics.txt` |
| C24 | k5 | `c24_alerts.py` | `check_rules_rc`, `test_rules_rc` |
| C25 | k5 | `c25_runbooks.py` | `alerts`: lista de `{"alert", "runbook"}` |
| C26 | k5 | `c26_density.py` | `limit_bytes`, `steps`: lista de `{"services", "wake_p99_ms", "memory_per_sleeping_kib", "errors"}`; crudo `c26/curve.csv` |
| C27 | k2 | `c27_lifecycle.py` | `before`, `during`, `after`, cada uno `{"netns": [...], "cgroups": [...], "mounts": [...], "nft": [...]}`. `netns` son rutas o procesos con «anden», `nft` las tablas visibles sin root y dentro del namespace |
| C28 | k5 | `c28_clean_build.py` | `command: "make check && make accept"`, `rc`; crudo `c28/build.log`; corre en un clon en `.run/acceptance/c28/clone` con `ANDEN_ACCEPT_SKIP=c28` y `ANDEN_CACHE` del repo original |
| C29 | k5 | `c29_ratio.py` | `product_loc`, `support_loc`, `ratio`; crudo `c29/cloc.json`; `acceptance/loc_paths.txt` declara las rutas de D27 |
| C30 | k6 | `c30_design.py` | `sections`: lista de títulos presentes en `vault/DESIGN.md`. Debe tener `## Decisiones`, `## Alternativas descartadas`, `## Deuda técnica conocida` y `## Lo que no se demostró` |

## 7. Conclusión permitida por medición

Declarado antes de medir, según el método:

| Medición | Contra | Conclusión permitida |
|---|---|---|
| C8 | Cifra pública de Railway | **distinto**: otra máquina y sin carga de plataforma. Solo se afirma «bajo 1 s aquí», no «más rápido que Railway» |
| C13, `relaunch` | runc, crun | **comparable**: Andén incluye proxy y red que esas filas no; es un costo, nunca un «más rápido» |
| C13, `restore` | `criu restore` directo | **equivalente** en el mecanismo; la diferencia es el costo de la plataforma |
| C13, `relaunch` | `restore` | **comparable**: garantías distintas (memoria conservada, sin seccomp) |
| C15 | Fuerza bruta | **equivalente**: mismo problema y objetivo; la brecha es real |
| C20 | umoci | **equivalente** en ruta, tipo, modo y hash; dueños fuera de la comparación |

## 8. Fuera de alcance

Definido en la idea; los límites concretos viven solo en `LIMITATIONS.md`.
