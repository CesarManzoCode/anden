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
| vegeta | 12.13.0 (paquete Arch) | `attack -rate -duration -keepalive -output`; `report -type=json` con `requests`, `success`, `status_codes`, `errors` |
| promtool | 3.16.0 (paquete Arch) | `check metrics` lee stdin, `check rules FILE`, `test rules FILE` |
| PostgreSQL | 18.6 (paquete Arch) | `initdb -D -A trust -U -N -E UTF8 --no-instructions`, `pg_ctl -D -w -l -o "-k DIR -c listen_addresses=''" start`, `-m immediate stop`; unos 31 MiB de RSS del postmaster |
| Valkey | 9.1.2 (paquete Arch) | `valkey-server --port --save ''`; `redis-cli` es enlace a `valkey-cli`, `SET` y `GET` responden |
| BIND `dig` | 9.20.30 (paquete Arch) | `dig @servidor nombre +short` según su ayuda |
| crun | 1.29.1 (`~/.local/bin`) | Suite runtime-tools y bundle con seccomp. El contrato usa `/usr/bin/crun`, versión de Arch 1.30.1 |
| runc | 1.5.1 (`~/.local/bin`) | `create` funciona; `start` y `run -d` no regresan. El contrato usa `/usr/bin/runc`, versión de Arch 1.5.2 |
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
