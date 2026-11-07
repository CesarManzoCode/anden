# Historial de cambios

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

- API gRPC publicada en `api/anden/v1/anden.proto`, con el código Go generado en `gen/anden/v1/`: servicio `Anden` para clientes y `AgentControl` para los agentes de nodo.
- `andend`, el plano de control: responde la API en un socket Unix y en TCP, guarda el estado en PostgreSQL por socket Unix y migra el esquema al arrancar. `restore` con volúmenes se rechaza con `FAILED_PRECONDITION`.
- `anden svc create|get|list|sleep|wake|delete|deploy|move`, que envuelve la API.
- `anden up`, `down`, `status`, `exec` y `chaos kill`: levantan y destruyen el datacenter simulado (scope de systemd delegado, Postgres, namespace de usuario con el plano de control, tres nodos y un cliente), corren un comando en el netns de un nodo, del cliente o raíz, y tiran un nodo con `cgroup.kill`.
- Criterio de aceptación C27 (`acceptance/c27_lifecycle.py`): ciclo de vida sin rastro.

- Contrato del sistema, con decisiones cerradas de lenguaje, arquitectura, rutas de despertar, proxy, simulador, colocación y fencing.
- Investigación con fuentes primarias y versiones de herramientas verificadas en la máquina de destino.
- Comando único de chequeo (`make check`) y CI que lo corre.

### Decisiones

- Todo el sistema corre sin root dentro de un namespace de usuario: se probó que red, nftables, overlayfs, cgroups delegados y CRIU funcionan así.
- La ruta de despertar `restore` corre sin filtro seccomp, porque CRIU sin root no puede volcar un proceso filtrado.
