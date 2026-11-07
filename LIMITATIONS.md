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
- Sin `sudo -n`, el listado de nftables de C27 solo ve lo que hay dentro del namespace del datacenter: las reglas del host no se pueden leer sin root. Andén tampoco puede escribirlas.
- La medición en caché fría de C8 necesita `sudo` y se omite con código 77 si falta.
- En C6, `swapon` no distingue si actuó el filtro seccomp: medido con `unshare -U --map-root-user` y sin filtro, ya da `EPERM`. Quien lo distingue es `mount`, que sin filtro da `ENOENT` sobre la ruta inexistente que usa `probe`.

## Conformidad OCI

- Solo se exige el subconjunto de 20 pruebas declarado en `acceptance/oci_subset.txt`. Quedan fuera hooks, límites de cgroup de la suite, AppArmor, etiquetas de montaje y mapeos de usuario anidados.
- `anden-runtime` rechaza en `create`, con un error, lo que está fuera de ese subconjunto: hooks, `process.terminal` y `--console-socket` con terminal, AppArmor, SELinux, `scheduler`, `ioPriority`, `execCPUAffinity`, montajes con idmap y opciones recursivas (`rro` y similares), `personality`, `intelRdt`, `memoryPolicy`, `netDevices`, `timeOffsets` y, de `linux.resources`, todo salvo `memory.limit`, `cpu.quota`, `cpu.period` y `pids.limit`.
- `anden-runtime` ignora `NOTIFY_SOCKET`: no reenvía `sd_notify` como runc. C6 quita la variable a ambos runtimes, así que esa diferencia no se compara.
- Sin `linux.uidMappings`, `anden-runtime` no crea un namespace de usuario: corre en el que ya existe aunque `config.json` pida uno.
- No une namespaces de usuario ni de tiempo por ruta ni crea namespaces de tiempo: un proceso Go es multihilo y `setns(2)` no admite esos cambios en un proceso multihilo.
- `linux.resources.devices` solo acepta la regla que lo niega todo, y no la aplica con cgroups (eso exige eBPF): el contenedor solo alcanza los nodos que el runtime monta. Sin root no hay `mknod`, así que los dispositivos son montajes bind de los nodos del host.
- Si el namespace de red no pertenece al namespace de usuario actual, `/sys` es un bind de solo lectura del `/sys` visible y no un sysfs propio.
- `linux.seccomp` solo admite las acciones `SCMP_ACT_ALLOW`, `SCMP_ACT_ERRNO` y `SCMP_ACT_KILL` (que mata el hilo), los operadores `SCMP_CMP_*` sobre argumentos y las banderas `SECCOMP_FILTER_FLAG_TSYNC`, `LOG` y `SPEC_ALLOW`. `create` rechaza cualquier otra acción, `listenerPath` y una lista `architectures` sin `SCMP_ARCH_X86_64`.
- El filtro solo cubre llamadas nativas de x86-64. Una llamada de otra arquitectura (i386) o x32 mata el proceso aunque `architectures` la liste, y un nombre que la tabla de x86-64 no conoce se ignora sin error, como hacen los runtimes basados en libseccomp con nombres de otras arquitecturas.
- Las reglas se evalúan en el orden del perfil y gana la primera que coincide. libseccomp ordena por su cuenta, así que un perfil con dos reglas distintas para la misma syscall puede comportarse distinto que en runc o crun.
- Con `process.noNewPrivileges` en `false`, el filtro se instala antes de soltar capacidades, porque sin `no_new_privs` el kernel exige `CAP_SYS_ADMIN`; runc hace lo mismo. Por eso el filtro ya rige sobre `setgroups`, `setresuid`, `capset` y `prctl` del propio runtime: un perfil que las bloquee hace fallar el arranque. Solo con `noNewPrivileges` en `true` se instala justo antes de `exec`.

## Imágenes

- Solo se admiten sha256 y capas `tar` o `tar+gzip`: una capa zstd o un manifiesto de Docker sin convertir se rechazan al importar.
- Los dispositivos de una capa se vuelven archivos vacíos con el modo de la entrada, igual que `umoci --rootless` 0.6.0; un contenedor los recibe como montajes bind del runtime, no de la imagen.
- No se aplican dueños, tiempos ni atributos extendidos de la capa. Una capacidad de archivo guardada en `security.capability` se pierde.
- Una entrada cuyo camino pasa por un symlink de la misma capa se rechaza aunque el destino quede dentro: seguirlo bien exigiría ver las capas inferiores.
- Un directorio padre que la capa no declara se crea con 0755 y, en el overlay, tapa el modo que tenía en la capa inferior. umoci conserva el de abajo. Las capas que escriben los constructores de imágenes declaran sus padres.
- Una entrada que cuelga de un whiteout de la misma capa (`.wh.d` seguido de `d/x` sin `d/`) falla al extraer.
- `anden image import` no vuelve a leer un blob que el almacén ya tiene: la copia guardada solo se revisa al extraerla o con `anden image verify`.
- Fuera del namespace del datacenter, `anden image mount` se reejecuta en un namespace de montaje propio y el montaje desaparece cuando el comando termina. Solo es útil dentro de un namespace que ya sea root, donde no hay reejecución.

## Plano de control

- La colocación sigue D07 (filtro por CPU, memoria y anti-afinidad, y best-fit por recurso dominante en orden decreciente de tamaño), pero es una heurística: no garantiza el máximo de servicios colocados. Su brecha frente a la fuerza bruta de `placeopt` es la que reporta C15. Un servicio que no cabe en ningún nodo se queda en `CREATING` sin error.
- El scheduler solo coloca lo que no tiene nodo. Un despertar sigue en su nodo hogar aunque ya no quepa: todavía no se elige otro nodo ni se mueve la dirección (D07).
- `placeopt` recorre (k+1)^n asignaciones, no las 3^n que dice D07: maximizar los servicios colocados exige considerar dejar uno fuera. Solo acepta hasta 12 servicios.
- En un despliegue, la instancia nueva va al mismo nodo que la vieja y el scheduler no le reserva CPU ni memoria: durante el despliegue el nodo puede quedar sobrecomprometido.
- Un singleton no hace rollout: `DeployService` para la instancia vieja y luego arranca la nueva, con tiempo sin servicio, para no tener nunca dos instancias activas.
- Un despliegue solo hace rollout si el servicio está `ACTIVE`. En cualquier otro estado (dormido, `CREATING`, `WAKING`, `FAILED`) solo cambia la versión deseada, como pide D24 para un servicio dormido.
- Un servicio admite un solo despliegue en curso: otro `DeployService` antes de que termine el draining responde `FAILED_PRECONDITION`.
- El plazo de `healthcheck_timeout_seconds` del plano de control cuenta desde `DeployService`, e incluye traer la imagen al nodo; el del agente cuenta desde que arrancó el proceso. Gana el primero que venza.
- Las fases del despliegue se miden con el reloj de pared de `andend` contra instantes guardados en Postgres. Un salto del reloj adelanta o atrasa el overlap y el draining.
- El draining termina a más tardar 10 s después de `draining_seconds` aunque la instancia vieja siga reportada, porque un nodo caído no vuelve a reportar.
- El SIGTERM le llega a la instancia vieja hasta un latido (500 ms) después del fin del overlap, y el draining se cuenta en el agente desde ese momento.
- Mientras dura un despliegue la instancia vieja no se duerme por inactividad. Si ya estaba dormida cuando la nueva pasó salud, no hay overlap: se drena de inmediato.
- `MoveService` deja el servicio dormido en su nuevo hogar. Una instancia despierta no pasa por el volcado: se para, así que un servicio `restore` pierde su memoria al moverse y despierta en el nodo nuevo por `relaunch`.
- Entre `MoveService` y la siguiente pasada del reconciliador (hasta 200 ms) el servicio no tiene dirección: el DNS responde NXDOMAIN y el borde no tiene su puerto. La dirección vieja queda libre en ese momento, aunque la instancia del nodo anterior todavía se esté parando.
- El DNS (`10.80.0.1:53`) solo conoce `<servicio>.anden.internal`: no reenvía a otro servidor ni hace recursión, responde NXDOMAIN a cualquier otro nombre y no tiene registros AAAA ni SRV. No tiene autenticación ni límite de consultas.
- El DNS lee la copia en memoria que el reconciliador renueva en cada pasada. Un cambio de dirección tarda hasta una pasada más el TTL de 1 s en llegar a un cliente, y tras reiniciar `andend` el DNS responde NXDOMAIN hasta la primera pasada.
- Una instancia que muere queda en `FAILED`, con su cgroup y su rootfs para inspección, y no se reinicia sola: la reemplaza `WakeService`, `DeployService` o `MoveService`.
- Cada latido lleva todas las asignaciones del nodo y lee Postgres. No está medido cuánto cuesta con muchos servicios.
- El servidor de blobs (`10.80.0.1:7444`) y la API para agentes no tienen autenticación: su única frontera es la red simulada.
- La vigencia de un nodo vive en la memoria de `andend`, con su reloj monótono. Tras reiniciar `andend`, un nodo del que no se supo nada cuenta como visto en el arranque, así que se declara muerto 4 s después y no antes.
- `GetService` y `ListServices` leen todas las instancias reportadas para armar la respuesta. No está medido cuánto cuesta eso con muchos servicios.

## Caída, partición y fencing

- El orden de D08 (autofencing a los 2 s, recolocación a los 4 s) está probado con relojes falsos en Go. En el datacenter solo lo miden tres corridas de C18 y tres de C19 (ver «Alcance de lo medido en K4»).
- El agente revisa su concesión cada 50 ms: el `cgroup.kill` llega entre 2 s y unos 2.05 s después de enviar el último latido aceptado, más lo que tarde el kernel. Un despertar o un arranque que ya corría en ese momento puede dejar procesos que el barrido no alcanza; esos se matan cuando ese despertar termina y la instancia se destruye, no en el barrido. No está medido cuánto tarda.
- El autofencing destruye todas las instancias del nodo, también las dormidas: su `upper/` se borra y el snapshot de `restore` se pierde. Tras una partición de entre 2 y 4 s, que el plano de control no alcanza a ver como caída, el nodo vuelve a arrancar desde cero lo que sigue asignado a él, con tiempo sin servicio.
- Si `andend` deja de responder latidos más de 2 s (un reinicio lento, Postgres caído, una sobrecarga), todos los agentes se autofencian y matan sus instancias, y las arrancan de nuevo al registrarse con una época nueva. No está medido cuánto tarda `andend` en volver tras un SIGKILL; C2 solo observa que el estado no cambió.
- Resultado negativo del 7 nov 2026: en el clon de C28, con otro proyecto cargando el disco y la CPU de la máquina, C26 falló con 500 servicios dormidos en un nodo. Un checkpoint de Postgres tardó 19 s en su `sync`, el latido esperaba detrás de intenciones y reportes que escriben, y el nodo se autofenció varias veces. Desde entonces el reporte va en un solo `COPY`, `andend` abre hasta 32 conexiones a Postgres y un latido aparte, que solo lee, renueva la concesión cuando el ciclo normal lleva medio TTL sin renovarla. Si las lecturas de Postgres o la respuesta de `andend` pasan de 2 s, el nodo se autofencia igual. Después del cambio C26 pasó dos veces con 1000 dormidos y sin errores, en el checker y dentro del clon de C28, sin medir la carga externa de la máquina en ninguna de las dos: el arreglo no está medido bajo la carga que provocó el fallo.
- `anden down` espera hasta 60 s a que el scope quede vacío tras `cgroup.kill`. Si no, falla y deja el estado; el scope desaparece cuando mueren sus procesos. En el fallo de arriba 10 s no bastaron, y C27, que corrió justo después, vio el scope de C26 en su lista de antes.
- Al registrarse con una época nueva, el plano de control borra lo que el nodo reportó con la anterior. Hasta su siguiente reporte (un latido), los servicios de ese nodo se ven como `WAKING`.
- Lo despierto de un nodo muerto se recoloca en un solo lote con el scheduler. Si no cabe en ningún nodo listo se queda sin nodo ni dirección, y el borde y el DNS lo pierden hasta que quepa.
- Lo dormido de un nodo muerto se recoloca con el scheduler sobre lo que reserva lo despierto; en esa decisión los dormidos sí se reservan entre sí, aunque después no reserven nada (D07). Si no cabe en ningún nodo listo se queda en el nodo muerto, sin poder despertar.
- Un singleton no recibe asignación en su nodo nuevo mientras un nodo vivo reporte una instancia suya de otra generación en `RUNNING` o `STARTING`. Si un nodo vivo reportara eso con la nueva ya corriendo, el nodo nuevo la pararía; no se encontró un camino que lo provoque, pero tampoco se midió.
- La identidad de un escritor de `writersvc` es aleatoria por proceso: un relanzamiento en el mismo nodo cuenta como otro escritor.
- Un volumen es un directorio de cada nodo, no un volumen compartido: C19 junta los `writers.log` de los tres nodos y los ordena por `t_ns`. Eso solo es válido porque todos escriben `CLOCK_MONOTONIC` del mismo kernel, lo que se razonó (no hay namespace de tiempo) y no se midió.

## Despertar

- Una conexión retenida después de que empezó a pararse el servicio (en la ruta `restore`, durante el volcado) no aborta el sueño: espera a que termine y a un despertar completo.
- Los contadores de `transitions.json` son por instancia: una instancia nueva del mismo servicio (despliegue, movimiento) los empieza de cero. El archivo no se borra al borrar el servicio.
- En C14 el retardo se mide desde que el nodo empieza el sueño, visto en `transitions.json`, no desde `SleepService`, que solo cambia el estado deseado. Arrancar `wakeclient` le suma unos milisegundos no medidos a cada retardo.

## Comparaciones

- En C13 las filas `runc`, `crun` y `criu-restore` corren sin límites de cgroup ni seccomp y sin proxy, sobre un overlay de una sola capa con `echosvc`; las de Andén tienen 1000 milicores y 128 MiB. Su reloj empieza antes de `create` o de `criu restore`, y el de Andén antes de `connect()`.
- La fila `criu-restore` lanza el contenedor con `anden-runtime` antes del volcado: el volcado y la restauración son directos, el arranque inicial no.
- Las filas sin proxy se miden reintentando la conexión cada milisegundo: esa granularidad entra en la muestra.
- C13 deja en `.run/acceptance/c13/work/` el directorio de trabajo de overlay con modo 000. La corrida siguiente lo abre antes de borrarlo; mientras tanto, el `find` de `make check` no lo puede leer e imprime `Permission denied`, aunque sale con 0.

## Instancias

- Un volumen es un directorio del nodo, `nodes/nodeN/volumes/<nombre>/`, montado con bind. Lo comparten los servicios del mismo nodo que usen ese nombre, sobrevive a dormir y despertar, no tiene cuota, no se borra con el servicio y no sigue a un servicio que cambie de nodo.
- El contenedor corre como uid 0 del namespace: el campo `User` de la imagen se ignora.
- Solo monta `/proc` y `/dev` (el `tmpfs` y los nodos del runtime). No hay `/sys`, `/dev/shm` ni `/dev/pts`. En la ruta `restore`, `/dev` no es un `tmpfs`: es el directorio del rootfs, en el `upper/` de la instancia, y los nodos se ligan sobre él, porque CRIU sin root no pudo restaurar por segunda vez un `tmpfs` que ya había restaurado.
- `memory.swap.max` queda en 0 en toda instancia: una instancia no usa swap aunque el nodo lo tenga.
- `/.anden/stdio.log` crece sin límite dentro del `upper` de la instancia, y el `stdout` del propio `anden-runtime create` va al mismo archivo.
- La lista de llamadas que bloquea el filtro seccomp es una decisión propia, sin un perfil de referencia detrás. Un servicio que necesite una de ellas recibe `EPERM`.
- La capacidad de un nodo simulado es un tercio de los CPU y de la memoria de la máquina, sin reservar nada para el plano de control ni para Postgres.

## Dormir y despertar

- La receta de D03 tal cual no basta dentro del agente: el volcado falla por los nodos de `/dev` y por el stdio abierto desde fuera del contenedor (ver RESEARCH). Los seis nodos que liga `anden-runtime` (`null`, `zero`, `full`, `random`, `urandom`, `tty`) pasan como montajes externos de CRIU y se vuelven a ligar desde el `/dev` del host al restaurar, y el contenedor abre `/.anden/stdio.log` y su `/dev/null` dentro de su raíz (anotación `anden.stdio`).
- Si la imagen no trae `/tmp`, `/proc`, `/dev` o `/dev/null`, el volcado los crea en el `upper/` de la instancia, porque son destino de un montaje de la vista.
- `criu restore` corre dentro de la cgroup de la instancia: su memoria y sus procesos cuentan contra `memory.max` y `pids.max` del servicio. No se midió con qué límites deja de caber.
- Solo cae a `relaunch` lo que falla antes de que `criu restore` termine bien. Un proceso restaurado que muere después deja la instancia en `FAILED`, como un arranque fallido.
- Dormida con snapshot, la instancia conserva sus namespaces de red, UTS e IPC y su veth en `svc0`, con su IP configurada. Sin snapshot los libera, como `relaunch`.
- El snapshot vive en la memoria del agente: si el agente se reinicia, o el servicio se mueve de nodo, el siguiente despertar es por `relaunch`. Las imágenes quedan en `snapshots/` hasta el siguiente volcado o el fin de la instancia.
- La API solo da `route`; `route_requested` solo está en `wake_events.jsonl` y en el registro del agente.
- Fuera de las fases quedan el saludo TCP antes del `accept` y lo que va del primer byte al último, que el cliente sí cuenta. Las fases las mide el propio agente con su reloj; C12 solo compara su suma con el cliente, no cada fase por separado.
- `placement` es solo la ida y vuelta de `SetAwake` al plano de control, con espera máxima de 200 ms. La respuesta no elige nodo todavía (D07, K4): con ella o sin ella, el despertar sigue en el mismo nodo. El latido manda después la misma intención otra vez, sin efecto.
- En la ruta `restore`, `network` solo abre el netns y la cgroup que el sueño conservó; no crea nada.
- Si la restauración falla y el despertar cae a `relaunch`, el tiempo de `criu restore` fallido se suma al `rootfs` del relanzamiento, porque cada fase cuenta desde la marca anterior.
- Solo deja evento un despertar que llega a `ACTIVE`. Si falla, o si la conexión que lo provocó se cierra antes, no queda línea.
- Un despertar sin conexión (`WakeService` o la orden del plano de control) deja su línea al llegar a `ACTIVE`, con `client_addr` vacío y `first_byte` en 0. Si la conexión que despertó al servicio termina sin recibir ningún byte, su línea sale al cerrar, también con `first_byte` en 0.
- Las conexiones que llegan durante un despertar ya iniciado esperan con él, pero no dejan línea propia: el evento es de la conexión que lo inició.
- `wake_events.jsonl` crece sin límite y se reabre en cada despertar. No se midió cuánto cuesta escribirlo; se escribe en otra gorrutina para no retrasar la respuesta.
- Sin snapshot, un servicio dormido deja su fila en Postgres, su elemento del set `sleeping`, el directorio de la instancia con solo `upper/` y `work/`, y su cgroup vacía. El prompt de k5-02 pide que quede solo la fila, el elemento y el directorio, pero C7 lee `cgroup.procs` y `memory.current` de esa cgroup después del sueño, así que se conserva. Su costo en memoria del kernel no se midió por separado: entra en lo que C26 atribuye a cada servicio dormido.
- Cada servicio dormido ocupa además memoria en los procesos que ya existen: su instancia y su máquina de despertar en el agente, y su registro DNS y su entrada del borde en `andend`. No abre procesos, montajes ni netns propios.
- Que las subredes `/16` y los puertos de borde (30000 a 32767) alcancen para 1000 servicios por nodo solo está probado en las pruebas de Go del asignador: C26 llega a 1000 servicios en todo el datacenter y no reporta cuántos quedan en cada nodo.
- El sueño conserva `upper/` y la cgroup. Tras un `relaunch` el servicio pierde su memoria, pero ve los archivos que escribió; `/.anden/stdio.log` sí empieza vacío en cada arranque.
- Un socket en `TIME_WAIT` no aborta el sueño, aunque el prompt del sprint pide abortar con todo lo que no sea `LISTEN`. Un `TIME_WAIT` no tiene proceso ni datos, y dura 60 s en el servicio que cerró primero, de modo que lo dejaría sin dormir ese tiempo. Cualquier otro estado sí aborta.
- `SleepService` sobre un servicio con una conexión viva no la corta: queda en `SLEEPING` y lo reintenta cada segundo hasta que la conexión termine.
- El estado que da la API va detrás del nodo: un despertar no espera al plano de control, que se entera en el siguiente latido.
- Si un despertar falla (el arranque da error o nada escucha en 10 s), la instancia queda en `FAILED`, las conexiones retenidas se cierran y las nuevas también, hasta que `WakeService` o `DeployService` la reemplacen. No hay reintento automático.
- Un `DeployService`, `MoveService` o `DeleteService` que llega durante un despertar cierra las conexiones retenidas en vez de pasarlas a la instancia nueva.
- Solo espera el despertar a que el puerto acepte conexiones TCP, como pide D04. El `healthcheck` del servicio se usa en el primer arranque, no al despertar.
- El despertar va siempre al nodo donde se durmió, sin comprobar que aún quepa (D07, K4).
- Solo se retienen conexiones TCP sobre IPv4: el set `sleeping` es `ipv4_addr`, y `eth0` de una instancia no tiene dirección IPv6.
- La señal de inactividad se muestrea a un cuarto de la ventana. Una conexión que abre y cierra entre dos muestras solo se ve por los paquetes que el servicio envió.
- El servicio ve como origen la dirección de `svc0` en las conexiones que pasan por el proxy, no la del cliente.

## Simulador

- `anden up` necesita `anden-agent` junto a `anden` en `bin/`. Sin él, `_dc-init` falla antes de arrancar `andend` y `up` sale con error.
- `anden down` solo borra un directorio de estado que tenga `dc.json` o `dc.log`. Si `up` falla, mata el scope pero deja el estado para inspeccionarlo hasta `anden down`.
- `_dc-init` reinicia `andend` cada vez que sale, sin espera ni límite de intentos.
- `anden node restart` no existe todavía: un nodo matado con `anden chaos kill` no vuelve, y sus filas de `instances` quedan en Postgres (el plano de control las ignora mientras el nodo esté muerto).
- `anden chaos slow` solo retrasa lo que sale del netns raíz hacia el nodo (la cola de salida de `veth-nodeN`), no lo que el nodo envía. `heal` quita también ese retardo, y no distingue si `tc qdisc del` falló porque no había retardo o por otra razón: ignora su error.
- Los procesos de `anden exec` entran al namespace de usuario y de red, no al de montaje del datacenter: ven los montajes del host.
- Solo el cliente externo y el netns raíz llegan al borde. Desde un nodo, una conexión a `10.80.0.2` agota el tiempo, incluso hacia un servicio de ese mismo nodo: la respuesta del servicio no pasa por el netns raíz y el DNAT no se deshace. Cada nodo tiene una ruta `/32` hacia el cliente para resolver esto solo para el cliente.
- Con esa ruta, el netns raíz emite redirecciones ICMP (`IcmpOutRedirects`) que el cliente no cuenta. No se investigó por qué.

## Mediciones

- Toda cifra vale para esta máquina (Ryzen 5 5600G, 14 GiB visibles, btrfs, gobernador `powersave`). No se compara con la de Railway como «más rápido»: la suya es de otra plataforma y de 2023.

## Métricas y alertas

- Los histogramas de despertar viven en la memoria de cada agente: si un agente se reinicia, sus conteos vuelven a cero. `wake_events.jsonl` sí persiste.
- El agregado de `andend` suma lo que responden los agentes en ese momento, con 1 s de espera por agente. Un agente caído o particionado queda fuera de la suma, y los conteos bajan como si se hubieran reiniciado.
- `anden_wake_duration_seconds` es la suma de fases que mide el agente desde su `accept` hasta el primer byte; no es el tiempo del cliente, que solo C12 compara con ella.
- Las alertas se prueban con `promtool test rules` sobre series sintéticas. Ningún Prometheus ni Alertmanager evalúa las reglas contra el datacenter.
- No hay comando para reiniciar el agente de un nodo caído, así que el runbook de `AndenNodeDown` solo puede esperar la recolocación o recrear el datacenter.

## Alcance de lo medido en K1

- C3 mide la conformidad solo contra runtime-tools en el commit `bd3b0bd00f44e15f0f1ff4130c61dbea1b1c56cc`. Otra versión de la suite no está medida.
- En la máquina, el efecto del filtro seccomp solo se ve en `mount` (C6) y en las 2 aserciones de `linux_seccomp` (C3). Las pruebas de Go del generador interpretan el BPF con un intérprete propio, no con el kernel.
- C6 compara contra `/usr/bin/runc` 1.5.2 y no contra crun.
- C20 compara solo alpine, nginx y la imagen sintética. Que el rootfs sea igual al de umoci no está medido en ninguna otra imagen.
- C21 solo mide un byte cambiado en una capa ya importada, y el rechazo al usarlo solo se observa en `anden image unpack`. No se midieron un blob truncado o faltante, ni un rechazo en `mount`.
- C22 mide la deduplicación con `du -sb` sobre `blobs/` y con un solo par de imágenes, nginx y nginx-perl, que comparten 7 capas. No mide con `du` las capas extraídas en `layers/<diffid>/`.

## Alcance de lo medido en K2

- C8 mide `echosvc` con una petición GET por despertar, sin carga generada por el script y por la ruta `relaunch`. No dice nada de un servicio que tarde más en escuchar.
- C8 en caché fría solo existe si `sudo -n` funciona en la corrida. `drop_caches` vacía la caché de toda la máquina, no solo la del servicio.
- C9 mide HTTP con `echosvc`: 32 clientes, 5 rondas y una conexión por petición. No mide conexiones que transfieran mucho ni que vivan mucho durante el despertar.
- C10 solo mide `PING`, `SET` y `GET` de `redis-cli` contra Valkey, con conexiones nuevas y sin persistencia: un `SET` no sobrevive al sueño con `relaunch`, y no se mide un servicio TCP con persistencia.
- Las cifras de K2 se midieron en seis corridas sobre seis árboles distintos, todas el 7 nov 2026, sin repetir ninguna sobre el mismo árbol ni hacer análisis estadístico. El `README.md` da la última (árbol `70b33e558d23`).
- Resultado negativo: en la corrida de C8 tibia sobre el árbol `a24a8a7b40a3` (19:12:47Z), hecha cuando la máquina estaba cargada, según el fixer de K5 por otro proyecto (sin medir), el p99 fue 271.2 ms y el máximo 1237.4 ms: una muestra pasó de 1 s. El criterio pasa porque mide el p99. En las otras cinco corridas el p99 estuvo entre 63.3 y 68.4 ms y el máximo entre 66.2 y 141.1 ms. «Bajo 1 s» vale para el p99, no para cada despertar, y no se midió con carga controlada.
- K2 cerró sin cifra de C8 en caché fría: `sudo -n` no estaba disponible.
- C1 recorre una vez el camino esperado con un solo servicio. `DeleteService` responde con el último estado (`ACTIVE`, no un estado de borrado), y C1 solo comprueba el código de esa llamada y el `NotFound` posterior: no mide que la instancia haya terminado.
- C2 mata `andend` una vez, con 3 servicios activos y sin operaciones en curso. No mide la caída de Postgres ni de un agente, ni un SIGKILL durante un despertar o con servicios dormidos.
- C4 mide un solo servicio con `memory.max` de 64 MiB que pide 1024 MiB, con otros dos bajo carga. No mide presión de memoria en todo el nodo ni en la máquina.
- C5 mide una bomba de fork con `pids.max` de 64, y la cuota de CPU con media CPU en una sola ventana de 5 s.
- C7 mide un `echosvc` y un solo sueño por inactividad. `memory.reclaim` puede responder `EAGAIN` sin liberar todo; el agente no lo trata como error, y lo que queda se ve solo en `memory.current`. La cifra de C7 vale para ese servicio, no para uno con más memoria.
- C8 despierta un servicio a la vez y cada muestra se duerme con `SleepService`, no por inactividad.
- C27 sube y destruye un datacenter sin servicios: no hay instancias, overlays, volúmenes ni elementos en el set `sleeping` que limpiar. Su lista de montajes lee el `mountinfo` del host, donde no aparecen los montajes del namespace privado del datacenter (`during` da vacío), así que no dice nada sobre esos montajes.

## Alcance de lo medido en K3

- Las cifras de K3 se midieron en cuatro corridas sobre cuatro árboles distintos, sin repetir ninguna sobre el mismo árbol ni hacer análisis estadístico. El `README.md` da la última (árbol `70b33e558d23`).
- Resultado negativo: en la corrida de C13 sobre el árbol `a24a8a7b40a3` (19:27:50Z), con la máquina cargada, `anden-restore` dio p90 de 157.1 ms y p99 de 230.4 ms, y `anden-relaunch` un máximo de 255.1 ms, frente a p99 de 38.0 a 40.0 ms y 64.4 a 66.6 ms en las otras tres corridas. Las filas de runc, crun y `criu restore` también se movieron (crun dio p90 de 84.6 ms, frente a unos 47 ms en las demás), pero ninguna tanto. No se investigó por qué la ruta `restore` fue la más sensible.
- Que la ruta `restore` conserve la memoria del proceso no lo mide ningún criterio de K3: C12, C13 y C14 solo miden tiempos y respuestas de `echosvc`. La conservación solo está observada a mano en la investigación de K0 (D03).
- C11 mueve una vez un solo servicio dormido, de node1 a node2, y lo despierta con `WakeService`. No mide un despertar provocado por una conexión después del movimiento, ni la ventana sin dirección entre `MoveService` y la siguiente pasada del reconciliador. Resuelve solo desde el netns `client`.
- C12 mide 100 despertares por ruta de `echosvc`, uno a la vez. La tolerancia (máx(5 ms, 10 %)) se cumple en los 200; no dice nada de despertares concurrentes ni de un servicio que tarde más en escuchar.
- C13 compara una sola imagen, `echosvc`, con n = 100 por fila. Las conclusiones (`comparable`, `equivalente`) son las declaradas antes de medir en el contrato, no el resultado de una prueba estadística. La diferencia de p50 entre `anden-restore` (32.3 ms) y `criu-restore` (16.4 ms) en la última corrida mezcla el costo de la plataforma con relojes que empiezan en sitios distintos y con límites de cgroup distintos, así que no aísla el costo de la plataforma.
- C14 inyecta 100 peticiones por ruta con retardo de hasta 20 ms desde el inicio del sueño. En la última corrida 97 por ruta cayeron dentro de la transición, y entre 96 y 99 en las anteriores; las demás se atendieron fuera de ella. «Siempre se atiende» vale para esas muestras, no como garantía.
- La verificación de C12 a C14 usa la receta de `restore` extendida por el fixer (ver «Dormir y despertar»), no la de D03 al pie de la letra.

## Alcance de lo medido en K4

- Las cifras de K4 se midieron en tres corridas sobre tres árboles distintos, sin repetir ninguna sobre el mismo árbol ni hacer análisis estadístico. El `README.md` da la última (árbol `70b33e558d23`). C15 dio lo mismo en las tres; C18 dio 4.70, 4.80 y 5.30 s sin servicio, y el hueco de C19 2.65, 2.64 y 2.65 s.
- C15 mide el scheduler fuera del datacenter, con `anden placement solve` sobre 50 instancias sintéticas de 3 nodos y 6 a 12 servicios. No mide la colocación que hace el reconciliador con servicios reales, ni instancias de más de 12 servicios, donde `placeopt` no sirve de comparación.
- En C15 el scheduler coloca 289 servicios frente a 362 de la fuerza bruta (73 menos) y coincide con el óptimo en 9 de 50 instancias. La conclusión `equivalente` es la declarada en el contrato antes de medir: dice que se comparó el mismo problema y objetivo, no que el scheduler sea igual de bueno.
- C16 y C17 miden un `echosvc` con una petición por conexión (`-keepalive=false`). No miden conexiones largas que crucen el cambio de versión ni el draining de una conexión abierta.
- C16 corre con overlap de 5 s y draining de 2 s, y C17 con `healthcheck_timeout_seconds` de 10 s, no con los defaults del producto (20 s, 20 s y 60 s). C17 solo mide un healthcheck HTTP que responde 503; no mide uno TCP, uno que agote el tiempo sin responder ni un rollback de un singleton.
- Tras el overlap, C16 toma 50 muestras y C17 20. Que «solo responde v2» vale para esas muestras.
- C18 mata un nodo una vez, con dos servicios despiertos que no son singleton; los dos fueron al mismo nodo (node2), así que no mide la anti-afinidad en la recolocación ni la recolocación de servicios dormidos. El tiempo sin servicio se mide con una sonda cada 100 ms, así que su resolución es de 100 ms.
- C19 particiona node1 una vez, durante 7.02 s, con un solo singleton. No mide una partición de entre 2 y 4 s (que el plano de control no ve como caída), ni `anden chaos slow`, que ningún criterio usa.
- En C19 el hueco entre escritores fue de 2.645 s: un singleton queda ese tiempo sin escritor activo. Que no haya solape se mide solo en esa corrida, con relojes de un solo kernel.

## Alcance de lo medido en K5

- Las cifras de K5 salen de la corrida que hizo el fixer sobre el árbol `70b33e558d23`; la verificación del cierre reutilizó esos informes. C26 corrió dos veces sobre ese árbol (en el checker y dentro del clon de C28), y las demás cifras una sola. No se midió cuánto varían entre corridas.
- C23 despierta un servicio por ruta, una vez cada uno. Mide que `/metrics` pase `promtool check metrics` y que existan las series de las 7 fases en las 2 rutas, no que los valores de los histogramas coincidan con lo que mide el cliente.
- C24 solo prueba las reglas con `promtool` sobre las series sintéticas de `ops/alerts_test.yml` (ver «Métricas y alertas»).
- C25 revisa que cada runbook exista, tenga sus encabezados y nombre solo comandos que da el uso de `bin/anden`. No ejecuta ningún runbook ni mide que resuelva la alerta.
- C26 mide `echosvc` de 50 milicores y 32 MiB, todos por `relaunch` y sin snapshot, hasta 1000 servicios: paró por el último escalón, no por el límite de 6 GiB, así que no hay cifra de más de 1000 dormidos ni de dónde se acaba la memoria. No mide la ruta `restore` dormida, que conserva sus namespaces.
- En C26 la memoria por dormido es lo que creció `memory.current` del scope sobre el datacenter sin servicios, dividido entre el número de dormidos. Se lee antes de los despertares de su escalón, pero incluye lo que dejaron los de escalones anteriores, y no separa el costo fijo del de cada servicio: por eso baja de 4012.4 KiB con 10 a 136.5 KiB con 1000. Entre 500 y 1000 dormidos el scope creció 35 586 048 bytes, 69.5 KiB por servicio agregado. Es memoria de la cgroup, no un desglose por proceso.
- El p99 de cada escalón de C26 sale de 50 despertares de uno en uno, así que es el máximo de la muestra. La curva no es monótona y cambia entre corridas: con 10 dormidos dio 251.8 ms en el checker y 329.7 ms en el clon. No mide despertares concurrentes con muchos dormidos.
- En C26 el p50 de despertar por escalón va de 135.9 a 173.7 ms, frente a 52.2 ms de C8 tibia con el mismo `echosvc`. No se investigó la diferencia.
- C28 clona solo lo que está en `HEAD`: no ve cambios sin commit. Comparte con el repositorio original `ANDEN_CACHE`, la caché de módulos y la de compilación de Go, así que no demuestra que las imágenes, runtime-tools ni los módulos se puedan traer desde cero ni sin red. Hereda el entorno de quien lo corre, salvo `ANDEN_ACCEPT_SKIP` y `ANDEN_CACHE`.
- C28 corre en un clon dentro de `.run/` del mismo repositorio, en la misma máquina y con el mismo usuario. Dentro del clon, C8 fría sale con 77 por falta de `sudo -n` y C28 se salta a sí mismo: «todas las pruebas» excluye esas dos. Tardó 2346.8 s.
- C29 cuenta el árbol de trabajo con cloc, no lo versionado: un archivo sin commit dentro de las rutas de `acceptance/loc_paths.txt` entra en la cuenta. La prueba de Go que revisa esas rutas vive en `acceptance/loc_paths_test.go`, un archivo que la tabla de rutas del contrato no lista en `acceptance/`; no suma líneas porque cloc excluye `*_test.go`.
- C29 pasa con apoyo menor que producto (razón 0.545). La referencia del método es 0.15: el apoyo es 3.6 veces esa referencia.

## Alcance de lo medido en K6

- C30 solo comprueba que `vault/DESIGN.md` exista y tenga sus cuatro secciones (`## Decisiones`, `## Alternativas descartadas`, `## Deuda técnica conocida` y `## Lo que no se demostró`). No comprueba que lo que dicen sea cierto ni que sus cifras coincidan con la evidencia.
- Las cifras del `README.md` y de `vault/DESIGN.md` se copiaron de la última corrida de cada criterio (árbol `70b33e558d23`); K6 no volvió a medir nada. Editar esos documentos cambia `lib.tree_stamp()`, así que los informes de esa corrida dejan de coincidir con el árbol y la siguiente verificación repite los scripts, con cifras que pueden diferir.
- La fracción 0.798 de C15 en el `README.md` (289 / 362) se calculó al escribirlo; no la reporta el script.
- El documento de diseño no lo revisó nadie de fuera del proyecto.
