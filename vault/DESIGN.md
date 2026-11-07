# Diseño de Andén

Andén es una plataforma de contenedores con scale-to-zero sobre tres nodos simulados en una sola máquina (Ryzen 5 5600G, 14 GiB visibles, kernel 7.2.14, btrfs, gobernador `powersave`). Todo el diseño gira alrededor de una cifra: el **presupuesto de arranque en frío**, el tiempo desde la primera petición a un servicio dormido hasta su primer byte de respuesta.

Este documento explica por qué el sistema es como es. Las cifras salen de la última corrida de cada criterio, sobre el árbol `70b33e558d23` el 7 nov 2026, salvo donde se dice otra cosa; la tabla completa está en el `README.md` y los límites en `LIMITATIONS.md`. El contrato (`vault/CONTRACT.md`) fija las decisiones; aquí se cuenta el razonamiento y lo que la medición dijo de ellas.

## Decisiones

### Todo corre sin root

Lo primero que se comprobó en la máquina fue qué se podía hacer sin privilegios. Dentro de `unshare -U --map-root-user --map-auto` funcionan bridges, veth, nftables con NAT y `redirect`, `SO_ORIGINAL_DST`, `tc netem`, overlayfs con `userxattr` sobre btrfs y CRIU 4.2.1; `systemd-run --user --scope -p Delegate=yes` entrega `cpu`, `memory` y `pids` con escritura real. Con eso, el datacenter entero (plano de control, Postgres, tres nodos y un cliente externo) cabe en un scope de systemd del usuario y un namespace de usuario.

Por qué importa: en esta máquina `sudo` pide contraseña, y una plataforma que exige root para cada prueba no se puede verificar de forma desatendida. El precio es real y está en `LIMITATIONS.md`: la ruta `restore` no lleva seccomp, las capas no conservan dueños y C8 en caché fría no se pudo medir. Un criterio que de verdad necesita root sale con 77; nunca se usó el 77 para evitar trabajo.

### Dormir es matar los procesos, no congelarlos

Un servicio inactivo se queda sin procesos: `cgroup.procs` vacío y `memory.current` en unos 500 KB (C7 midió 499 712 bytes en la última corrida). Se descartó el freezer de cgroups porque deja los procesos vivos y su memoria sigue ocupada; para la densidad que persigue scale-to-zero, lo que no corre no debe pesar.

Inactivo significa: en toda la ventana `idle_seconds` no hubo sockets `ESTABLISHED` ni `SYN_RECV` y no creció `tx_packets` de su veth. Son las dos señales públicas que se conocen: una conexión abierta e inactiva no se corta (Fly.io) y el tráfico saliente cuenta como actividad (Railway).

### Despertar con un proxy L4 que retiene la conexión

Cada nodo tiene un set `sleeping` de nftables (`dirección . puerto`) y una regla que redirige a `:15000` todo lo que vaya a un servicio dormido. El agente acepta, lee el destino original con `SO_ORIGINAL_DST`, despierta el servicio, sondea TCP cada 2 ms y copia bytes en ambos sentidos. Al terminar saca el destino del set: las conexiones nuevas van directo y las ya establecidas siguen por el proxy hasta cerrarse, porque conntrack conserva su redirección.

Por qué así: funciona con cualquier TCP, no solo HTTP (C10 duerme y despierta Valkey con `redis-cli` sin configuración especial), nunca corta una conexión viva, y el cliente no tiene que reintentar nada. C9 lo midió con 32 clientes concurrentes en 5 rondas: 1600 peticiones, ninguna perdida ni duplicada. C14 inyectó 100 peticiones por ruta mientras el servicio se dormía, sin errores ni timeouts.

### Dos rutas de despertar, medidas entre sí

- `relaunch` (por defecto) crea el netns y arranca el proceso desde cero. Sirve para todo servicio.
- `restore` restaura un volcado de CRIU. Conserva memoria, pero no admite volúmenes ni seccomp.

Si el volcado o la restauración fallan, el servicio cae a `relaunch` y la ruta usada queda registrada. Tener las dos no es redundancia: es la comparación que pide el proyecto, y la caída evita que un snapshot perdido deje un servicio sin despertar.

Medido (C13, n = 100 por fila, p50 / p99): `anden-relaunch` 55.3 / 66.0 ms y `anden-restore` 32.3 / 39.6 ms, frente a `runc` 38.0 / 70.7 ms, `crun` 46.8 / 79.4 ms y `criu restore` directo 16.4 / 17.4 ms. La conclusión permitida se declaró antes de medir: `relaunch` es *comparable* a runc y crun (Andén incluye proxy y red que esas filas no), y `restore` es *equivalente* a `criu restore` en el mecanismo. Nada de esto dice «más rápido que».

### Desglose por fases que se puede auditar

Cada despertar deja una línea en `wake_events.jsonl` con siete fases (`detection`, `placement`, `rootfs`, `network`, `start`, `readiness`, `first_byte`). Un desglose que nadie contrasta no vale; C12 compara la suma de las fases con lo que mide un cliente externo, y en 200 de 200 despertares (100 por ruta) quedó dentro de máx(5 ms, 10 %). Una corrida posterior falló: emparejó solo 99 de 100 despertares `restore`, porque el `redirect` a `:15000` había reescrito el puerto del cliente al chocar en conntrack con una conexión anterior a otro servicio. Desde entonces el agente toma `client_addr` de la tupla original en `/proc/net/nf_conntrack`. Las mismas fases alimentan los histogramas `anden_wake_phase_seconds` y `anden_wake_duration_seconds` y la alerta `AndenWakeOverBudget`.

### Estado deseado durable y un reconciliador de nivel

`andend` guarda estado deseado y observado en PostgreSQL 18 por socket Unix, y un reconciliador compara uno con otro en cada pasada, sin colas. Postgres vive fuera del árbol de los nodos, así que un SIGKILL del plano de control no pierde nada: C2 lo mata con servicios en los tres nodos y `anden status` y los procesos de las instancias quedan idénticos. Un reconciliador de nivel, a diferencia de uno por eventos, se recupera de cualquier estado intermedio con solo volver a mirar.

### Fencing por concesión: el nodo se mata a sí mismo antes de que otro lo reemplace

El agente renueva una concesión cada 500 ms con TTL de 2 s medido con su reloj monótono. Si no la renueva, se autofencia: `cgroup.kill` de todas sus instancias. El plano de control declara muerto el nodo a los 4 s y solo entonces recoloca. Caída y partición se tratan igual porque desde fuera no se distinguen.

La razón del orden: como el fencing termina a los 2 s y la recolocación empieza a los 4 s, dos instancias de un singleton no pueden solaparse mientras ambos relojes monótonos avancen igual, algo cierto en una sola máquina. C19 particionó el nodo de un singleton durante 7.02 s: el escritor viejo dejó de escribir antes de que arrancara el nuevo, con un hueco de 2.65 s sin escritor. C18 mató un nodo y sus dos servicios volvieron en otro con 5.30 s sin servicio, frente al límite de 10 s.

### Colocación heurística, contrastada contra la fuerza bruta

El scheduler filtra por CPU, memoria y anti-afinidad, y elige best-fit por recurso dominante, del servicio más grande al más pequeño. Solo reserva lo despierto: lo dormido no ocupa capacidad. Se eligió una heurística simple porque es predecible y barata en cada pasada del reconciliador, y se midió contra `placeopt`, una fuerza bruta exacta para hasta 12 servicios.

El resultado es honesto y no favorable: en 50 instancias nunca violó capacidad ni anti-afinidad, pero colocó 289 servicios frente a 362 de la fuerza bruta, y alcanzó el óptimo en solo 9 (C15).

### Borde y DNS propios

Un punto de entrada estable (`10.80.0.2`, un puerto por servicio desde 30000, con DNAT en una sola transacción `nft -f -`) permite que vegeta mida rollouts sin cambiar de destino. Un DNS en `andend` responde `<servicio>.anden.internal` con TTL 1 hacia la dirección del servicio, despierto o dormido, porque resolver el nombre de un servicio dormido debe despertarlo. C11 lo movió dormido de node1 a node2 y `dig` dio la dirección nueva.

### Rollout con overlap y draining

`DeployService` arranca la versión nueva junto a la vieja; cuando pasa su healthcheck cambia borde y DNS en un paso, espera `overlap_seconds`, manda SIGTERM a la vieja y SIGKILL tras `draining_seconds`. Si el healthcheck no pasa, destruye la nueva sin tocar el borde. Es la semántica que Railway documenta. C16 y C17, con vegeta a 200 peticiones/s durante 30 s, dieron 6000 peticiones sin errores en un rollout y en un rollback.

### Runtime e imágenes propios, comparados con herramientas de referencia

`anden-runtime` implementa `create`, `start`, `state`, `kill` y `delete` con namespaces, cgroups v2, seccomp compilado a BPF sin bibliotecas y overlayfs. Su conformidad se declara como un subconjunto: las 20 pruebas de runtime-tools que crun pasa limpias en este mismo entorno (C3: 20 de 20). Así el piso lo fija un runtime de referencia y no una opinión. El almacén usa el formato OCI layout, verifica el digest al importar, transferir y extraer, y guarda cada capa una vez (C22: nginx y nginx-perl comparten 7 capas); su rootfs es igual al de `umoci` en ruta, tipo, modo y hash (C20).

### Lenguajes y presupuesto de verificación

El producto es Go con `CGO_ENABLED=0`, que permite el patrón de reejecución para entrar a namespaces sin C. Los verificadores son Python con solo biblioteca estándar y clientes de medición en Go que nunca importan `internal/`: miden el producto desde fuera. El código de apoyo debe ser menor que el de producto; C29 midió 9347 líneas de producto y 5095 de apoyo, razón 0.545.

## Alternativas descartadas

- **Freezer de cgroups para dormir.** Despertar sería más rápido, pero los procesos siguen vivos y su memoria ocupada. Viola el objetivo de C7 y la densidad de C26.
- **Una VM por nodo.** El fallo de nodo se inyecta igual con `cgroup.kill` y `ip link set down`, y una VM resta RAM al banco de densidad sin aportar nada a C18 ni C19.
- **Firecracker como fila de comparación.** Exige kernel de invitado y rootfs ext4 aparte, y mide otra garantía (aislamiento de VM). No se agregó; está fuera de alcance.
- **Proxy en HTTP o L7.** Limitaría el scale-to-zero a HTTP. El proxy L4 con redirección del kernel cubre cualquier TCP.
- **Esperar el primer byte con sondeos espaciados.** Railway describe un piso cercano a 30 ms; sondear cada 2 ms baja ese piso a cambio de unos pocos `connect` fallidos durante el arranque.
- **Colas o un bus de eventos en el plano de control.** Un reconciliador de nivel sobre Postgres no pierde trabajo si `andend` muere y no agrega otro sistema que operar.
- **Distinguir caída de partición.** No se puede desde fuera; tratar ambas igual hace que el fencing sea la única garantía contra dos escritores.
- **Un solver de optimización para colocar.** La fuerza bruta es exacta hasta 12 servicios y no depende de un solver instalado; el scheduler del producto es una heurística barata y su brecha queda medida.
- **Verificar el rootfs en cada despertar.** Costaría presupuesto de arranque sin que ningún criterio lo pida; `anden image verify` recorre todo bajo demanda.
- **Usar `runc start` para medir runc.** En este entorno `runc start` no regresa aunque el contenedor arranca. La medición de C13 la hace un cliente externo, así que no depende de ese retorno.
- **Exigir sudo donde no hace falta.** Dejaría sin demostrar C3 a C6, C13, C18, C19 y C27 en una corrida sin contraseña.

## Deuda técnica conocida

- **El despertar no cambia de nodo.** D07 pide que un despertar que no cabe en su nodo hogar vaya a otro; no está implementado. La fase `placement` solo hace la ida y vuelta al plano de control.
- **Un nodo matado no vuelve.** `anden node restart`, que el contrato lista, no existe.
- **El snapshot de `restore` vive en la memoria del agente.** Si el agente se reinicia, o el servicio se mueve o se recoloca, el siguiente despertar es por `relaunch` y se pierde la memoria.
- **El autofencing depende de que Postgres y `andend` respondan en menos de 2 s.** Con la máquina cargada por otro proyecto, un checkpoint de Postgres tardó 19 s y un nodo con 500 servicios dormidos se autofenció varias veces sin que nadie lo particionara. Se agregaron un latido de solo lectura, un reporte con un solo `COPY` y un pool de 32 conexiones, pero el arreglo no se midió bajo esa carga.
- **El scheduler queda lejos del óptimo.** 289 de 362 servicios en C15. Una heurística mejor, o reacomodar en vez de colocar solo lo que no tiene nodo, queda pendiente.
- **La receta de `restore` del contrato no basta.** El agente pasa los nodos de `/dev` como montajes externos de CRIU, el contenedor abre su stdio dentro de su raíz y en esa ruta `/dev` no es un `tmpfs`. El contrato no se actualizó.
- **Desde un nodo no se llega al borde.** Solo el netns `client` y el raíz alcanzan `10.80.0.2`; el DNAT no se deshace para tráfico de un nodo.
- **Archivos que crecen sin límite.** `wake_events.jsonl` y `/.anden/stdio.log` no rotan.
- **Costos sin medir.** Cada latido lleva todas las asignaciones del nodo y lee Postgres, y `GetService` lee todas las instancias. Con muchos servicios no se midió cuánto cuestan.
- **APIs internas sin autenticación.** El servidor de blobs y la API para agentes confían en la red simulada como única frontera.
- **El código de apoyo pesa.** La razón apoyo/producto es 0.545, 3.6 veces la referencia de 0.15 del método.

## Lo que no se demostró

- **C8 en caché fría.** Necesita `sudo -n` para vaciar la page cache y no estuvo disponible en ninguna corrida: salió con 77 siempre. Solo hay cifra en caché tibia.
- **Que el despertar quede bajo 1 s con la máquina cargada.** En una corrida sobre el árbol `a24a8a7b40a3`, mientras la máquina estaba cargada, C8 tibia dio p99 de 271.2 ms y un máximo de 1237.4 ms: una muestra pasó de 1 s. El criterio pasa porque mide el p99. En las otras cinco corridas del mismo criterio, el p99 estuvo entre 63.3 y 68.4 ms.
- **Variación entre corridas.** Hay pocas corridas por criterio y no se hizo ningún análisis estadístico. C18 dio 4.70, 4.80 y 5.30 s sin servicio en tres corridas; C26 dio 251.8 ms de p99 con 10 dormidos en una corrida y 329.7 ms en otra sobre el mismo árbol.
- **Que `restore` conserve la memoria.** Ningún criterio lo mide: C12, C13 y C14 miden tiempos y respuestas. Solo se observó a mano en la investigación inicial.
- **Densidad más allá de 1000 dormidos.** C26 paró en el último escalón, no en el límite de 6 GiB: no se sabe dónde se acaba la memoria. Tampoco mide la ruta `restore` dormida ni despertares concurrentes.
- **Por qué el despertar en C26 es más lento que en C8.** El p50 por escalón de C26 va de 135.9 a 173.7 ms, frente a unos 55 ms de C8 con el mismo servicio. No se investigó.
- **Relación con Railway.** Su única cifra pública es «menos de 1 s», de 2023 y de otra plataforma. Aquí solo se afirma «bajo 1 s en esta máquina», no «más rápido que Railway».
- **Fallos reales.** Caída y partición son inyecciones de software en una sola máquina; el fencing supone relojes monótonos que avanzan igual, algo que entre hosts reales no se cumple sin tolerar deriva.
- **Que la colocación del reconciliador sea la de C15.** C15 mide el scheduler fuera del datacenter, con instancias sintéticas; C18 recolocó dos servicios en el mismo nodo y no ejercitó la anti-afinidad.
- **Que un clon se construya desde cero.** C28 comparte la caché de imágenes, de módulos y de compilación con el repositorio original, y se salta C8 fría y a sí mismo.
- **Que los runbooks resuelvan sus alertas.** C25 revisa que existan y que sus comandos existan; las alertas solo se probaron con `promtool` sobre series sintéticas.
