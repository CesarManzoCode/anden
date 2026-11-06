# Límites

Fuente única de lo que Andén no demuestra. Ningún otro archivo los repite.

## Fuera de alcance por diseño

- Migración en vivo entre hosts reales.
- BGP y eBPF de producción, malla WireGuard.
- Replicación de volúmenes.
- Escala de flota.
- Fallas de hardware real: NIC, disco, energía.
- Latencia y ancho de banda reales entre hosts.
- microVMs: no hay fila de comparación con Firecracker.

## Consecuencias de correr en una sola máquina

- El fencing confía en que los relojes monótonos del agente y del plano de control avanzan igual. En una máquina es cierto; entre hosts reales habría que tolerar deriva.
- Caída y partición de nodo son inyecciones de software. No prueban un fallo de energía ni de red física.
- La red entre nodos es un bridge del kernel. Sus latencias no representan un enlace entre hosts.

## Consecuencias de correr sin root

- La ruta `restore` corre sin filtro seccomp: CRIU sin root no puede volcar un proceso filtrado. La API lo reporta por instancia.
- La ruta `restore` no admite volúmenes.
- CRIU no puede volcar sockets MPTCP. Un servicio Go debe correr con `GODEBUG=multipathtcp=0` para usar `restore`; si el volcado falla, el servicio cae a `relaunch` y pierde su memoria.
- Las capas extraídas sin root no preservan dueños reales con la misma fidelidad, y por eso C20 no compara dueños.
- El listado de nftables de C27 solo ve lo que hay dentro del namespace. Las reglas del host no se pueden leer sin root; Andén tampoco puede escribirlas.
- La medición en caché fría de C8 necesita `sudo` y se omite con código 77 si falta.
- En C6, `swapon` no distingue si actuó el filtro seccomp: medido con `unshare -U --map-root-user` y sin filtro, ya da `EPERM`. Quien lo distingue es `mount`, que sin filtro da `ENOENT` sobre la ruta inexistente que usa `probe`.

## Conformidad OCI

- Solo se exige el subconjunto de 20 pruebas declarado en `acceptance/oci_subset.txt`. Quedan fuera hooks, límites de cgroup de la suite, AppArmor, etiquetas de montaje y mapeos de usuario anidados.
- `anden-runtime` rechaza en `create`, con un error, lo que está fuera de ese subconjunto: hooks, `process.terminal` y `--console-socket` con terminal, AppArmor, SELinux, `scheduler`, `ioPriority`, `execCPUAffinity`, montajes con idmap y opciones recursivas (`rro` y similares), `personality`, `intelRdt`, `memoryPolicy`, `netDevices`, `timeOffsets` y, de `linux.resources`, todo salvo `memory.limit`, `cpu.quota`, `cpu.period` y `pids.limit`.
- Sin `linux.uidMappings`, `anden-runtime` no crea un namespace de usuario: corre en el que ya existe aunque `config.json` pida uno.
- No une namespaces de usuario ni de tiempo por ruta ni crea namespaces de tiempo: un proceso Go es multihilo y `setns(2)` no admite esos cambios en un proceso multihilo.
- `linux.resources.devices` solo acepta la regla que lo niega todo, y no la aplica con cgroups (eso exige eBPF): el contenedor solo alcanza los nodos que el runtime monta. Sin root no hay `mknod`, así que los dispositivos son montajes bind de los nodos del host.
- Si el namespace de red no pertenece al namespace de usuario actual, `/sys` es un bind de solo lectura del `/sys` visible y no un sysfs propio.
- `linux.seccomp` solo admite las acciones `SCMP_ACT_ALLOW`, `SCMP_ACT_ERRNO` y `SCMP_ACT_KILL` (que mata el hilo), los operadores `SCMP_CMP_*` sobre argumentos y las banderas `SECCOMP_FILTER_FLAG_TSYNC`, `LOG` y `SPEC_ALLOW`. `create` rechaza cualquier otra acción, `listenerPath` y una lista `architectures` sin `SCMP_ARCH_X86_64`.
- El filtro solo cubre llamadas nativas de x86-64. Una llamada de otra arquitectura (i386) o x32 mata el proceso aunque `architectures` la liste, y un nombre que la tabla de x86-64 no conoce se ignora sin error, como hacen los runtimes basados en libseccomp con nombres de otras arquitecturas.
- Las reglas se evalúan en el orden del perfil y gana la primera que coincide. libseccomp ordena por su cuenta, así que un perfil con dos reglas distintas para la misma syscall puede comportarse distinto que en runc o crun.
- Con `process.noNewPrivileges` en `false`, el filtro se instala antes de soltar capacidades, porque sin `no_new_privs` el kernel exige `CAP_SYS_ADMIN`; runc hace lo mismo. Por eso el filtro ya rige sobre `setgroups`, `setresuid`, `capset` y `prctl` del propio runtime: un perfil que las bloquee hace fallar el arranque. Solo con `noNewPrivileges` en `true` se instala justo antes de `exec`.

## Mediciones

- Toda cifra vale para esta máquina (Ryzen 5 5600G, 14 GiB visibles, btrfs, gobernador `powersave`). No se compara con la de Railway como «más rápido»: la suya es de otra plataforma y de 2023.

## Sin demostrar todavía

Todo el producto. Esta sección se reduce conforme cada K cierre.

- `anden-runtime` está escrito (k1-02) y su generador de BPF para seccomp también (k1-03), pero no se han ejecutado en esta máquina: ni su conformidad OCI ni su aislamiento están medidos hasta que corran C3 y C6. Las pruebas de Go del generador interpretan el programa con un intérprete propio, no con el kernel.
