# Investigación

Fuentes primarias con su revisión fijada, consultadas el 6 nov 2026. Una página sin control de versiones se fija por su fecha de consulta.

## Herramientas ejecutadas en esta máquina

`pacman` no estaba al día: Prometheus 3.15.1 ya no existía en los espejos y se probó 3.16.0. Las herramientas sin instalar se ejecutaron extrayendo su paquete oficial a un directorio temporal, sin instalarlas.

| Herramienta | Versión probada | Qué se comprobó |
|---|---|---|
| Go | go1.27.3 | Compila binarios estáticos, `go list -m`, `go install` |
| protoc | libprotoc 36.1 | `--version` |
| Python | 3.14.9 | Scripts con `-I` |
| GNU Make | 4.4.1 | `--version` |
| util-linux `unshare` | sistema | `--map-root-user`, `--map-auto`, `--kill-child`, `--mount-proc` |
| nftables | 1.1.7 | Set concatenado, `redirect to :puerto`, mapa de NAT con `dnat ip to tcp dport map @svc`, `add element` y `delete element` en una transacción, todo sin root en un namespace |
| iproute2 | 7.3.0 | bridge, veth, `netns`, `tc qdisc add ... netem delay`, alternar `link set down/up` |
| cloc | 2.10 | `--json --quiet --not-match-f='_test\.go$' --fullpath --not-match-d=...`, clave `SUM.code` |
| CRIU | 4.2.1 (paquete Arch) | Recetas de D03 ejecutadas de punta a punta; `--help`; `--network-lock skip` |
| umoci | 0.6.0 (paquete Arch) | `unpack --rootless --image DIR:TAG bundle` deja `rootfs/`, `config.json`, `umoci.json`, `sha256_*.mtree` |
| skopeo | 1.24.1 (paquete Arch) | `copy --override-os linux --override-arch amd64 docker://REPO@sha256:... oci:DIR:TAG`, `inspect`. Una referencia con tag y digest falla («Docker references with both a tag and digest are currently not supported»): el tag solo nombra la copia. El layout queda con la anotación `org.opencontainers.image.ref.name` en el tag dado y apunta al manifiesto de amd64, no al índice |
| glibc `ldd` | sistema | `LIB => /ruta (0x...)`; `/lib64/ld-linux-x86-64.so.2 => /usr/lib64/ld-linux-x86-64.so.2 (0x...)`, donde la izquierda es el intérprete del binario; `linux-vdso.so.1 (0x...)` sin ruta; sobre un binario estático, `not a dynamic executable` con salida 1 |
| vegeta | 12.13.0 (paquete Arch) | `attack -rate -duration -keepalive -output`; `report -type=json` con `requests`, `success`, `status_codes`, `errors`. El 7 nov 2026, `vegeta attack -h` de `/usr/bin/vegeta` 12.13.0 lista además `-targets` (archivo de objetivos, formato `http` por defecto: `GET URL` por línea), `-header` (cabecera de petición, repetible; el README del proyecto usa la forma `"Nombre: valor"`) y `-timeout`; `vegeta report` lee el archivo de resultados como argumento. `-header` y `-targets` no se ejecutaron contra un servicio |
| promtool | 3.16.0 (paquete Arch) | `check metrics` lee stdin, `check rules FILE`, `test rules FILE` |
| PostgreSQL | 18.6 (paquete Arch) | `initdb -D -A trust -U -N -E UTF8 --no-instructions`, `pg_ctl -D -w -l -o "-k DIR -c listen_addresses=''" start`, `-m immediate stop`; unos 31 MiB de RSS del postmaster |
| Valkey | 9.1.2 (paquete Arch) | `valkey-server --port --save ''`; `redis-cli` es enlace a `valkey-cli`, `SET` y `GET` responden |
| BIND `dig` | 9.20.30 (paquete Arch) | `dig @servidor nombre +short` según su ayuda |
| crun | 1.29.1 (`~/.local/bin`) | Suite runtime-tools y bundle con seccomp. El contrato usa `/usr/bin/crun`, versión de Arch 1.30.1 |
| runc | 1.5.1 (`~/.local/bin`) | `create` funciona; `start` y `run -d` no regresan. El contrato usa `/usr/bin/runc`, versión de Arch 1.5.2. Medido con 1.5.2 el 6 nov 2026: con `NOTIFY_SOCKET` en el entorno (lo hereda de la sesión de systemd), `start` enlaza `<root>/<id>/notify/notify.sock`; con un `--root` cuya ruta total da 115 bytes, más que `sun_path`, falla con `bind: invalid argument` y el contenedor queda en `created`. Con ruta corta, `start` sobre un servicio largo no regresó en 5 s; sin la variable regresó en 5 ms. C6 quita la variable; C13 tendrá que hacer lo mismo |
| systemd-run | sistema | `--user --scope -q -p Delegate=yes` |

## Dependencias Go (`go list -m ...@latest`)

| Módulo | Versión |
|---|---|
| `google.golang.org/grpc` | v1.85.0 |
| `google.golang.org/protobuf` (y `protoc-gen-go`) | v1.36.12 |
| `google.golang.org/grpc/cmd/protoc-gen-go-grpc` | v1.6.2 |
| `github.com/jackc/pgx/v5` | v5.12.0 |
| `golang.org/x/sys` | v0.49.0 |
| `github.com/miekg/dns` | v1.1.73 |
| `github.com/prometheus/client_golang` | v1.25.0 |

## Imágenes reales (digest del índice, consultado con `skopeo inspect`)

| Imagen | Digest | Capas |
|---|---|---|
| `docker.io/library/alpine:3.23` | `sha256:f3d55900449227ae9e237ede521e7b665d53a253b42065ba90b9db9275e198ad` | 1 |
| `docker.io/library/debian:trixie-slim` | `sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2` | 1 |
| `docker.io/library/nginx:1-trixie` | `sha256:165ddde8751e75a1c907d24ef10d9f6deb08b57e0fe56caf67512486aff76d82` | 7 |
| `docker.io/library/nginx:1-trixie-perl` | `sha256:82a12bb0001413a9c1e243f67dbc6e742b59b537f88841a0c8b865a7a271aea5` | 8; comparte las 7 de arriba |

## Especificaciones y código con revisión

| Fuente | Revisión | Para qué |
|---|---|---|
| [opencontainers/runtime-tools](https://github.com/opencontainers/runtime-tools) | `bd3b0bd00f44e15f0f1ff4130c61dbea1b1c56cc` (v0.9.0 + commits, 17 oct 2026) | C3. Compila con `make runtimetest validation-executables`, salida TAP, `RUNTIME=...`. CLI de `docs/command-line-interface.md`: `create --bundle --console-socket --pid-file`, `start`, `state`, `kill --signal`, `delete`. Su Makefile define `VALIDATION_TESTS ?= $(patsubst %.go,%.t,$(shell find ./validation/ -name *.go \| grep -v util))` |
| [opencontainers/image-spec](https://github.com/opencontainers/image-spec/tree/v1.1.1) | tag `v1.1.1`, `de7a13cc5ce022e0486d22211e80eb36f9d4a218` | `verify/cmd/mkimage`: `oci-layout` con `imageLayoutVersion`, `index.json`, `blobs/sha256/`, tipos `application/vnd.oci.image.{index,manifest,config}.v1+json` y `...layer.v1.tar[+gzip]`, anotación `org.opencontainers.image.ref.name`, whiteouts `.wh.` y opaco en `layer.md` |
| Go, `src/internal/godebugs/table.go` | go1.27.3 instalado | `multipathtcp` es un ajuste conocido de `//go:debug` (paquete `net`) |
| [opencontainers/runtime-spec](https://github.com/opencontainers/runtime-spec/blob/469dd5b6233c9d0137d16be792978aebb302666e/runtime.md) | `469dd5b6233c9d0137d16be792978aebb302666e` (rama `main`); `runc --version` declara spec 1.3.0, tag `v1.3.0` | Estados y operaciones |
| [checkpoint-restore/criu](https://github.com/checkpoint-restore/criu/blob/v4.2.1/Documentation/criu.txt) | tag `v4.2.1`, `f3e4ef5389601ed0893820d5eef1a769a5eee901` | `--unprivileged`, `--root`, `--external net[inode]:name`, `--inherit-fd`, `--manage-cgroups`, `--network-lock` (iptables, nftables, skip). La página [criu.org/CLI/opt/--network-lock](https://criu.org/CLI/opt/--network-lock) está desactualizada y no menciona `skip`. `criu/proc_parse.c` explica por qué hace falta la vista de D03 |
| [opencontainers/umoci](https://umo.ci/quick-start/rootless/) | tag `v0.6.0`, `7b958b02bd52c2c9342081eca52ef7855a06602f` | C20 |
| [actions/checkout](https://github.com/actions/checkout) | `v7.0.1`, `3d3c42e5aac5ba805825da76410c181273ba90b1` | CI |
| [actions/setup-go](https://github.com/actions/setup-go) | `v7.0.0`, `b7ad1dad31e06c5925ef5d2fc7ad053ef454303e`; entrada `go-version` | CI |

## Runtime OCI (consultado el 6 nov 2026 para k1-02)

| Fuente | Revisión | Dato usado |
|---|---|---|
| runtime-tools, `validation/util/container.go` | `bd3b0bd00f44e15f0f1ff4130c61dbea1b1c56cc` | La suite invoca `create --pid-file P --bundle B ID`, `start ID`, `state ID`, `kill ID SEÑAL` (la señal va posicional; `--signal` está comentado porque runc no lo admite) y `delete [--force] ID`. El stdout y stderr del contenedor son los de `create`, redirigidos a archivos del bundle. `Clean()` hace `delete --force` sobre contenedores creados o detenidos |
| runtime-tools, `generate/generate.go` y `validation/*/` | misma | La configuración por defecto trae `linux.resources.devices` con una sola regla que niega todo, namespaces pid, network, ipc, uts y mount, y el perfil seccomp por defecto. `linux_ns_nopath` crea los siete namespaces con `uidMappings` y `gidMappings` (hostID 1000, containerID 0, tamaño 1000), aunque D09 excluya `linux_uid_mappings`. `linux_ns_path` une ipc, mnt, net, pid (vía `pid_for_children`) y uts; `linux_ns_path_type` espera error cuando la ruta es de otro tipo, también para `user`. `start` con `process` ausente debe terminar sin error y dejar el contenedor detenido |
| [runtime-spec, `specs-go/config.go` y `state.go`](https://github.com/opencontainers/runtime-spec/blob/469dd5b6233c9d0137d16be792978aebb302666e/specs-go/config.go) | `469dd5b6233c9d0137d16be792978aebb302666e` | Nombres JSON de todos los campos, que el parseo estricto declara; `state` con `ociVersion`, `id`, `status`, `pid`, `bundle`, `annotations` |
| [setns(2), man7.org](https://man7.org/linux/man-pages/man2/setns.2.html) | consulta del 6 nov 2026 | No se entra a un namespace de montaje compartiendo atributos de `CLONE_FS`; un proceso multihilo no cambia de namespace de usuario ni de tiempo; `setns` de PID solo afecta a los hijos; tipo distinto de `nstype` da `EINVAL` |
| `/usr/include/linux/nsfs.h` y `/usr/include/asm/unistd_64.h` | sistema | `NS_GET_NSTYPE = _IO(0xb7, 0x3)`; `__NR_setns 308` en x86-64 |
| Go, `src/syscall/exec_linux.go`, `exec_unix.go`, `rlimit.go`; `src/os/exec_posix.go` | go1.27.3 instalado | `SysProcAttr` tiene `Cloneflags`, `UidMappings`, `GidMappingsEnableSetgroups`, `AmbientCaps` y `UseCgroupFD`/`CgroupFD` (este último fuerza `clone3` con `CLONE_INTO_CGROUP`); el hijo se clona desde el hilo que llama; `syscall.Exec` restaura el `RLIMIT_NOFILE` guardado al arrancar salvo que antes se llame `syscall.Setrlimit`; `os.StartProcess` llama `Fd()` sobre cada archivo heredado, lo que lo deja bloqueante. `syscall` no define `SYS_SETNS` |

## Seccomp (consultado el 6 nov 2026 para k1-03)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `/usr/include/linux/seccomp.h`, `filter.h`, `bpf_common.h`, `audit.h`, `elf-em.h` | sistema, kernel 7.2.14 | `SECCOMP_SET_MODE_FILTER 1`; banderas `TSYNC 1`, `LOG 2`, `SPEC_ALLOW 4`; retornos `KILL_PROCESS 0x80000000`, `KILL_THREAD 0` (= `SECCOMP_RET_KILL`), `ERRNO 0x00050000`, `ALLOW 0x7fff0000`, `DATA 0xffff`; `struct seccomp_data` es `int nr`, `u32 arch`, `u64 instruction_pointer`, `u64 args[6]`; `struct sock_filter` es `u16 code, u8 jt, u8 jf, u32 k`; `BPF_MAXINSNS 4096`; opcodes `BPF_LD 0x00`, `BPF_ALU 0x04`, `BPF_JMP 0x05`, `BPF_RET 0x06`, `BPF_W 0`, `BPF_ABS 0x20`, `BPF_K 0`, `BPF_AND 0x50`, `BPF_JEQ 0x10`, `BPF_JGT 0x20`, `BPF_JGE 0x30`; `AUDIT_ARCH_X86_64 = EM_X86_64(62) \| 0x80000000 \| 0x40000000` |
| `/usr/include/asm/unistd_64.h` y `asm/unistd.h` | sistema | Tabla de 386 syscalls de x86-64 copiada a `internal/runtime/sysnum_linux_amd64.go`; `__NR_seccomp 317`, `__NR_mount 165`, `__NR_swapon 167`; `__X32_SYSCALL_BIT 0x40000000` |
| runtime-tools, `generate/seccomp/seccomp_default.go`, `parse_action.go`, `syscall_compare.go`, `validation/linux_seccomp/linux_seccomp.go` | `bd3b0bd00f44e15f0f1ff4130c61dbea1b1c56cc` | El perfil por defecto usa `SCMP_ACT_ERRNO` por defecto, arquitecturas `X86_64`, `X86`, `X32`, una lista de `SCMP_ACT_ALLOW` con nombres de varias arquitecturas, `personality` con `SCMP_CMP_EQ` y, sin `CAP_SYS_ADMIN`, `clone` con `SCMP_CMP_MASKED_EQ` (`value` es la máscara, `valueTwo` el dato). `linux_seccomp` pone `allow` por defecto, lo que borra las reglas `allow`, y agrega `getcwd` con `errno` |
| runtime-tools, `cmd/runtimetest/main.go` | misma | Comprueba que `prctl(PR_GET_NO_NEW_PRIVS)` coincida con `process.noNewPrivileges`, así que el runtime no puede activar `no_new_privs` por su cuenta para instalar el filtro |
| [runtime-spec, `config-linux.md`](https://github.com/opencontainers/runtime-spec/blob/469dd5b6233c9d0137d16be792978aebb302666e/config-linux.md#seccomp) | `469dd5b6233c9d0137d16be792978aebb302666e` | Campos `defaultAction`, `defaultErrnoRet`, `architectures`, `flags`, `listenerPath`, `syscalls[].names`, `action`, `errnoRet` (por defecto `EPERM`; con una acción sin errno el runtime debe fallar), `args[].index`, `value`, `valueTwo`, `op` |
| `runc --help`, `runc create --help`, `runc delete --help` | `/usr/bin/runc` 1.5.2, libseccomp 2.6.0 | `--root DIR` global, `create --bundle B ID`, `start ID`, `state ID`, `delete --force ID`; `--rootless` es `auto` por defecto |

## Almacén de imágenes (consultado el 6 nov 2026 para k1-04)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `umoci unpack --rootless`, ejecutado con una imagen de dos capas hecha a mano | `/usr/bin/umoci` 0.6.0 | Un char o block device queda como archivo regular vacío con el modo de la entrada (666 y 640) y un aviso `rootless{...} creating empty file in place of device`: no se omite la ruta. Un archivo con `.wh.` del mismo nombre en su misma capa sigue visible. Un directorio con `.wh.` en su misma capa conserva lo de esa capa y oculta lo de abajo. Conserva los modos 4755 y 000 |
| [umoci, `oci/layer/tar_extract.go`](https://github.com/opencontainers/umoci/blob/v0.6.0/oci/layer/tar_extract.go) | tag `v0.6.0` | La rama `tar.TypeChar, tar.TypeBlock` con `te.partialRootless` crea el archivo vacío; `partialRootless` vale `Rootless \|\| inUserNamespace` |
| [image-spec, `layer.md`](https://github.com/opencontainers/image-spec/blob/v1.1.1/layer.md) | tag `v1.1.1` | «Whiteout files MUST only apply to resources in lower/parent layers»; lo que está en la misma capa solo lo ocultan whiteouts de capas posteriores |
| Go, `src/archive/tar/reader.go`, `src/internal/godebugs/table.go` | go1.27.3 instalado | `Reader.Next` solo devuelve `ErrInsecurePath` con `GODEBUG=tarinsecurepath=0`; por defecto entrega el nombre tal cual y validarlo le toca a quien extrae. No valida destinos de enlaces |

## API y estado (consultado el 6 nov 2026 para k2-01)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `make gen` ejecutado en esta máquina | protoc 36.1 (el encabezado generado dice `protoc v7.36.1`), `protoc-gen-go` v1.36.12, `protoc-gen-go-grpc` v1.6.2 | Genera `gen/anden/v1/anden.pb.go` y `anden_grpc.pb.go` con `paths=source_relative`; los campos `optional` de proto3 salen como punteros (`*uint32`) |
| pgx, `pgconn/config.go` | `github.com/jackc/pgx/v5` v5.12.0 | Un `host` que empieza con `/` es un directorio de socket Unix y la dirección es `<dir>/.s.PGSQL.<puerto>`; los valores entre comillas simples admiten escapes con `\` |
| pgx, `conn.go` | v5.12.0 | `Exec` sin argumentos usa siempre el protocolo simple, que acepta varias sentencias en una llamada; `BeginFunc` confirma o revierte la transacción |
| grpc, `internal/resolver/unix/unix.go` | `google.golang.org/grpc` v1.85.0 | El destino `unix:///ruta/absoluta` toma la ruta del URL; una autoridad no vacía es error |
| `go get` y `go mod tidy` | go1.27.3 | grpc v1.85.0 arrastra `golang.org/x/sys` v0.48.0 como indirecta; pgxpool necesita `github.com/jackc/puddle/v2` v2.2.3. El proxy de Go solo respondió por IPv4: con IPv6 dio `network is unreachable` |

## Simulador (consultado el 6 nov 2026 para k2-02)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `systemd-run --help`, `man systemd-run`, ejecutado | systemd 262 | `-u, --unit=UNIT`; con `--scope` la unidad se llama `UNIT.scope` y el proceso queda en `/user.slice/user-<uid>.slice/user@<uid>.service/app.slice/UNIT.scope` (leído de `/proc/self/cgroup`). Tras salir, `systemctl --user show -P ControlGroup UNIT.scope` todavía imprime la ruta aunque el directorio ya no exista |
| `systemctl --help`, `man systemctl` | systemd 262 | `-P NAME` es `--value --property=NAME`; `reset-failed UNIT`; `stop UNIT` |
| `man unshare` | util-linux 2.42.4 | `--propagation` vale `private` por defecto con `--mount`; `--kill-child[=signame]` |
| `nsenter --help`, `man nsenter` | util-linux 2.42.4 | `-t PID`, `-U`, `-n`, `-m`; `--preserve-credentials` no toca uid ni gid al entrar al namespace de usuario |
| `lsns --help`, ejecutado | util-linux 2.42.4 | `-t net -J -o NS,PID,COMMAND` da `{"namespaces": [{"ns", "pid", "command"}]}` |
| `man ip`, `man ip-link` | iproute2 7.3.0 | `-b, -batch FILE` lee de un archivo o de la entrada estándar y para en el primer error; `ip link add DEV type veth peer name NAME`; `ip link set DEV netns PID`; `ip -n` solo acepta un nombre de `/run/netns`, no un pid, por eso se usa `nsenter -t PID -n ip` |
| `sysctl --help` | procps-ng (sistema) | `-q -w net.ipv4.ip_forward=1` |
| `sleep --help` | coreutils (sistema) | Documenta un número entero o decimal, no `infinity`: los procesos que sostienen un netns usan `sleep 2147483647` |
| `rm --help` | coreutils (sistema) | `--one-file-system` |
| `/proc/config.gz` | kernel 7.2.14 | `CONFIG_PROC_CHILDREN=y`: existe `/proc/<pid>/task/<tid>/children` |
| `/proc/self/status` | kernel 7.2.14 | `NSpid:` lista el pid en cada namespace de pid, del más externo al propio |
| `man nft` | nftables 1.1.7 | `list tables [family]`, `list ruleset` |

## Agente de nodo (consultado el 6 nov 2026 para k2-03)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `unshare`, ejecutado en un namespace de usuario y de montaje, y `man unshare` | util-linux 2.42.4 | `--net=F --uts=F --ipc=F true` deja los tres namespaces persistentes como montajes `nsfs` sobre `F`. El archivo debe existir antes y la ruta debe ser absoluta: sin el archivo falla con `No such file or directory`. El montaje debe tener propagación no compartida, y es `private` tras `unshare --mount` y un `mount --bind D D` |
| `ip -batch -` y `nsenter`, ejecutados | iproute2 7.3.0, util-linux 2.42.4 | `link set DEV netns /ruta/net.ns` acepta una ruta absoluta a un archivo de namespace que exista; con el archivo ausente da `Invalid "netns" value`. `nsenter --net=/ruta/net.ns -- ip -batch -` configura adentro. La IP responde `ping` desde `svc0`. `ip link del` borra el par, y `umount -l` libera los archivos |
| `nft -f -`, ejecutado | nftables 1.1.7 | El guion que crea `table ip anden_edge {}`, la borra y la vuelve a declarar con su mapa corre dos veces seguidas sin error, así que sirve aunque ya exista. `delete element ... { PUERTO }` más `add element` cambia el valor de una clave en una transacción |
| grpc, `dialoptions.go`, `backoff.go`, `backoff/backoff.go`, `clientconn.go` | `google.golang.org/grpc` v1.85.0 | `WithNoProxy`, `WithConnectParams(ConnectParams{Backoff: backoff.Config{BaseDelay, Multiplier, Jitter, MaxDelay}, MinConnectTimeout})`. El `BaseDelay` por defecto es 1 s y el máximo 120 s. `grpc` registra el resolvedor `passthrough` |
| `/sys/fs/cgroup/.../memory.swap.max`, `/proc/swaps` | kernel 7.2.14 | El archivo existe en la cgroup del usuario y la máquina tiene swap en `/dev/zram0`, de ahí que una instancia lo ponga en 0 para que exceder `memory.max` termine en OOM |
| Go, `net/http.ServeMux` (`go doc`) | go1.27.3 | Patrones con método y comodín, y `Request.PathValue` |

## Proxy de despertar (consultado el 6 nov 2026 para k2-04)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `nft -f -`, ejecutado en un namespace de usuario y de red | nftables 1.1.7 | `table inet anden_wake` con el set concatenado y `redirect to :15000` en `prerouting` y `output` se puede declarar dos veces seguidas. `add element` repetido no falla; `delete element` de lo que no está falla con `No such file or directory`, por eso la máquina recuerda si el destino está en el set |
| Conexiones reales con Python, en ese namespace | kernel 7.2.14 | Con `10.81.0.3 . 8080` en el set, un `connect` desde el mismo netns llega a `:15000` y `getsockopt(SOL_IP, 80)` devuelve 16 bytes `02001f900a510003...`: familia en orden del host, puerto y dirección en orden de red. Con `SO_MARK = 0x414e` y la regla `meta mark 0x414e return` antes de la de D04, el `connect` llega al servicio real. Sin esa regla, el sondeo y el puente del propio agente volverían al proxy, porque salen por `output`; D04 no lo menciona |
| `ip -batch -`, ejecutado | iproute2 7.3.0 | `link set eth0 address 02:00:0a:51:00:02` y `link set eth0 addrgenmode none`, antes de `up`, dejan la MAC fija y ninguna dirección IPv6 en `eth0` |
| `/proc/self/net/tcp` y `/proc/self/net/dev`, leídos | kernel 7.2.14 | La cuarta columna de `tcp` es el estado en hexadecimal (`0A` = `LISTEN`), tras una línea de encabezado. En `dev`, tras dos líneas de encabezado y `nombre:`, hay ocho columnas de recepción, y luego bytes y paquetes transmitidos |
| Go, `go doc` | go1.27.3 | `net.ListenConfig.SetMultipathTCP(false)` vence el MPTCP que Go activa por defecto en los listeners; `syscall.SO_MARK` existe |

Lo no medido aquí: la cifra de 2 ms de sondeo es el intervalo pedido por D04, no una latencia observada. Los códigos de estado distintos de `LISTEN` se tomaron de `include/net/tcp_states.h` sin observarlos uno por uno. Que la MAC fija evite una espera por caché ARP al despertar es hipótesis: se razonó por el funcionamiento de NUD, no se comparó contra una MAC nueva.

## Cliente de medición y C8 a C10 (consultado el 6 nov 2026 para k2-05)

| Herramienta o archivo | Qué se comprobó en esta máquina |
|---|---|
| `findmnt` (util-linux) | `findmnt -no FSTYPE --target RUTA` imprime `btrfs` para el repositorio; `--help` documenta `-n`, `-o` y `-T, --target` |
| `/sys/devices/system/cpu/cpu0/cpufreq/scaling_governor` | Se lee sin root e imprime `powersave` |
| `sudo -n true` | Sale con 1 y «a password is required»: aquí C8 en caché fría sale con 77 |
| `/proc/sys/vm/drop_caches` | No ejecutado, por falta de `sudo`. D18 fija escribir 3 |
| `valkey-server` 9.1.2 | `--port P --bind 0.0.0.0 --protected-mode no --save ''` arranca: «Ready to accept connections tcp». `/etc/valkey/valkey.conf` trae `protected-mode yes` y `bind 127.0.0.1 -::1` |
| `redis-cli` | `--help` documenta `-h`, `-p` y `--no-raw` («Force formatted output even when STDOUT is not a tty»), o sea que sin tty la salida es cruda |

## Cierre de k2 (medido el 6 nov 2026)

| Herramienta o archivo | Qué se comprobó en esta máquina |
|---|---|
| `memory.reclaim` de una cgroup vacía, kernel 7.2.14 | Con `memory.current` en 331776 bytes, escribir ese valor con `os.write` de Python da `EAGAIN`, y escribir 1048576 también; escribir `0` no da error |
| `os.WriteFile` de Go sobre `memory.reclaim`, go1.27.3 | No regresa cuando el kernel responde `EAGAIN`: la pila del agente, obtenida con `SIGQUIT`, queda en `internal/poll.(*FD).Write`, en `waitWrite`. Con `syscall.Open` y `syscall.Write` el `EAGAIN` llega al llamador |
| Borde con `10.80.0.100` y los nodos en la misma `/24` | Sin ruta propia, el nodo responde directo por `eth0`: `curl` desde `client` a `10.81.0.2:8080` responde y a `10.80.0.2:30000` agota el tiempo. Con `ip route add 10.80.0.100/32 via 10.80.0.1` en el nodo, responde 200 en unos 0.2 ms |
| `nstat`, tras 21 peticiones por el borde | El netns raíz cuenta 5 `IcmpOutRedirects`; el cliente, 0 `IcmpInRedirects`, y `ip route show cache` en el cliente queda vacío. Por qué el cliente no los cuenta no se investigó |
| `curl` a `10.80.0.2:30000` desde `node1`, `node2` y `root` | `node1`, donde corre el servicio, y `node2` agotan 2 s; `root` responde 200 |

## Ruta restore (consultado el 6 nov 2026 para k3-01)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `criu --help` | `/usr/bin/criu` 4.2.1 | `-t\|--tree PID`, `-d\|--restore-detached`, `-R\|--leave-running` (sin él, las tareas no siguen tras el volcado), `--pidfile FILE` escribe el pid de la tarea raíz, `-W` toma por defecto el valor de `-D` (ahí quedan los registros de `-o`), `--unprivileged`, `--external RES`, `--inherit-fd fd[NUM]:RES`. La ayuda no lista el formato `net[inode]` de `--external`: viene de D03 y de `criu.txt` |
| Go, `src/runtime/os_linux.go` | go1.27.3 instalado | Los hilos del runtime se crean con `CLONE_FS`: un `chroot` desde cualquier hilo vale para todos |
| Go, `src/syscall/exec_linux.go` | go1.27.3 instalado | Con `UseCgroupFD` usa `clone3` con `CLONE_INTO_CGROUP` y conserva `Cloneflags` (por ejemplo `CLONE_NEWNS`) en las banderas de `clone3` |

No se ejecutó nada de la ruta dentro del agente en esta sesión.

## Desglose por fases (consultado el 6 nov 2026 para k3-02)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `time.get_clock_info('monotonic')`, ejecutado | Python 3 del sistema | `implementation='clock_gettime(CLOCK_MONOTONIC)'`, resolución 1e-09: `time.monotonic_ns` lee el mismo reloj que el agente escribe en `t_accept_ns` |
| `/usr/include/linux/time.h` | cabeceras instaladas | `CLOCK_MONOTONIC` vale 1 |
| Go, `src/syscall/zsysnum_linux_amd64.go` y `go doc syscall.Timespec.Nano` | go1.27.3 | `SYS_CLOCK_GETTIME = 228`; `(*Timespec).Nano() int64`. `time.Now` guarda una lectura monótona pero no expone su valor |

Que los netns del datacenter compartan ese reloj con el host se razonó (no se crea namespace de tiempo), no se midió.

## DNS y movimiento (consultado el 6 nov 2026 para k3-03)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `github.com/miekg/dns`, código del módulo en la caché (`server.go`, `defaults.go`, `types.go`, `client.go`) | v1.1.73 | `Server{PacketConn, Listener, Handler}` con `ActivateAndServe`; `Shutdown` falla con `server not started` si el servidor no arrancó todavía, y cerrar el socket termina `serveUDP` y `serveTCP` con error; `Msg.SetReply` copia el id, el opcode y la primera pregunta y deja `RcodeSuccess`; `RcodeNameError` es NXDOMAIN; `A{Hdr: RR_Header{Name, Rrtype, Class, Ttl}, A net.IP}`; `Client{Net: "udp"\|"tcp", Timeout}.Exchange(m, addr)` |
| `go get github.com/miekg/dns@v1.1.73` con `GOPROXY=off` y `go mod tidy -diff` | go1.27.3 | Sus requisitos (`x/net` v0.57.0, `x/sync` v0.22.0, `x/sys` v0.47.0) son menores que los del módulo, así que no suben ninguna versión; `go.sum` solo suma las dos líneas de `miekg/dns` |
| `dig -h` | BIND 9.20.30 | `dig [@global-server] [domain] ... +[no]short` |

No se ejecutó el servidor DNS ni C11 en esta sesión.

## Carrera y comparación (consultado el 6 nov 2026 para k3-04)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `crun --help`, `crun kill --help`, `crun delete --help` | `/usr/bin/crun` 1.30.1 | `--root=DIR` global; `kill CONTAINER [SIGNAL]`; `delete -f, --force` |
| `nsenter --help` | util-linux 2.42.4 | `-u, --uts[=<file>]`, `-i, --ipc[=<file>]`, `-n, --net[=<file>]` entran a un namespace por archivo; `-t`, `--preserve-credentials` |
| `unshare --help` | util-linux 2.42.4 | `-m, --mount`; `--propagation` (privada por defecto, ver arriba) |
| `chroot --help` | coreutils (sistema) | `chroot NEWROOT [COMMAND [ARG]...]`; cambia a `/` salvo con `--skip-chdir` |
| `/usr/include/linux/prctl.h` | cabeceras instaladas | `PR_SET_CHILD_SUBREAPER 36` |
| `criu --version` | `/usr/bin/criu` 4.2.1 | La versión de la receta de D03 |

No se ejecutó C13 ni C14 en esta sesión. Que runc y crun funcionen dentro del namespace de usuario y de pid del datacenter, con `nsenter -U -n -p` y un scope delegado propio, es hipótesis: C6 y C3 los corrieron con el prefijo de D09, que crea namespaces nuevos.

## Ruta restore dentro del agente (ejecutado el 6 nov 2026, fixer de k3)

| Fuente | Revisión | Dato usado |
|---|---|---|
| `criu --help` | `/usr/bin/criu` 4.2.1 | `--external mnt[MOUNTPOINT]:COOKIE` al volcar y `--external mnt[COOKIE]:ROOT` al restaurar |
| `dump.log` del agente, servicio `echosvc` con `wake_route = restore` | criu 4.2.1, kernel 7.2.14 | Sin externos, el volcado falla con `mnt: 593:./dev/tty doesn't have a proper root mount`: los nodos que `anden-runtime` liga desde el `/dev` del host no tienen en el contenedor el devtmpfs de donde vienen |
| El mismo `dump.log` con los seis nodos como `mnt[/dev/<nodo>]:dev-<nodo>` | ídem | Falla con `Can't lookup mount=544 for fd=1 path=.../merged/.anden/stdio.log`: el stdio que el agente abre desde fuera pertenece a un montaje de su namespace, no del contenedor. Si el contenedor lo abre dentro de su raíz (anotación `anden.stdio`), el volcado y la restauración terminan bien |
| `restore.log` del segundo despertar | ídem | La restauración de un árbol que ya se había restaurado falla con `Unable to mount tmpfs .../dev: Invalid argument`. Sin `/dev` en tmpfs (los nodos se ligan sobre el `/dev` del rootfs) se midieron cuatro despertares seguidos por `restore` |

**[hipótesis]** La causa del `EINVAL` sería que el kernel muestra el `uid=` y `gid=` de un tmpfs en ids del host, y criu vuelve a montar con un uid que el namespace del datacenter no mapea. No se leyó el código del kernel ni se midió.

En todas las corridas, `criu` deja `rlimit: Can't setup RLIMIT_NOFILE for self: Operation not permitted` como error y sigue.

## Documentación oficial sin revisión fija (consultada el 6 nov 2026)

| Fuente | Dato |
|---|---|
| [Prometheus, promtool](https://prometheus.io/docs/prometheus/latest/command-line/promtool/) y [pruebas de reglas](https://prometheus.io/docs/prometheus/latest/configuration/unit_testing_rules/) | `check metrics` por stdin; claves `rule_files`, `evaluation_interval`, `tests`, `input_series`, `alert_rule_test`, `eval_time`, `alertname`, `exp_alerts` (vacío significa que no dispara) |
| [Kernel, overlayfs](https://docs.kernel.org/filesystems/overlayfs.html) | `-o userxattr`, whiteout char device 0/0, directorio opaco |
| [Kernel, cgroup v2](https://docs.kernel.org/admin-guide/cgroup-v2.html) | Delegación y regla de no procesos internos. La consulta cubrió solo la primera parte de la página; los archivos usados (`cgroup.kill`, `memory.events`, `memory.reclaim`, `pids.max`, `cpu.max`, `cpu.stat`) se comprobaron además por existencia y escritura en esta máquina |
| [nftables, sets](https://wiki.nftables.org/wiki-nftables/index.php/Sets) y [NAT](https://wiki.nftables.org/wiki-nftables/index.php/Performing_Network_Address_Translation_(NAT)) | `add element`, `redirect to`. Los conjuntos concatenados y `delete element` no aparecen en esas páginas: se verificaron ejecutándolos |

## Comparables y cifras públicas

| Fuente | Dato | Uso |
|---|---|---|
| [Railway, scale-to-zero](https://blog.railway.com/p/launch-week-01-scale-to-zero) (29 sep 2023) | «consistently observe < 1 second to get a complete HTTP response with a few hundred bytes of data on cold boot». Es una observación, no un SLA. Sondeo TCP cada 30 ms hasta 10 s | Umbral de C8 y modelo de D04 |
| [Railway, deployment teardown](https://docs.railway.com/deployments/deployment-teardown) | `RAILWAY_DEPLOYMENT_OVERLAP_SECONDS`, `RAILWAY_DEPLOYMENT_DRAINING_SECONDS`; SIGTERM y luego SIGKILL. La página no da valores por defecto | D24 |
| [Vacante de Railway](https://railway.com/careers/infra-platform) | Requisitos y ejercicio de entrevista | Propósito |
| [Firecracker, snapshots](https://github.com/firecracker-microvm/firecracker/blob/85cd69df7fe503961cb191263bb1045b96c29ff4/docs/snapshotting/snapshot-support.md) | Sin cifra de restauración; «depends on the memory size, vCPU count and emulated devices count» | D16 |
| [gVisor, rendimiento](https://gvisor.dev/docs/architecture_guide/performance/) | Sin cifra de arranque en texto; solo un gráfico | D16 |

## Lo no encontrado

- Si Railway usa hoy snapshots de memoria o SOCKMAP en producción.
- Cifras de arranque en frío de Railway más recientes que 2023.

La investigación adjunta del proyecto, fuera de este repositorio, trae más contexto sobre la vacante y comparables secundarios que no se usan como referencia numérica.
