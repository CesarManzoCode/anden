# Historial de cambios

## Sin publicar

### Agregado

- Contrato del sistema, con decisiones cerradas de lenguaje, arquitectura, rutas de despertar, proxy, simulador, colocación y fencing.
- Investigación con fuentes primarias y versiones de herramientas verificadas en la máquina de destino.
- Comando único de chequeo (`make check`) y CI que lo corre.
- `anden-runtime`, runtime OCI con `create`, `start`, `state`, `kill` y `delete` según la interfaz de línea de comandos de runtime-tools: namespaces nuevos o por ruta, rootfs con `pivot_root`, montajes, dispositivos, capacidades, rlimits, usuario, sysctl, `oomScoreAdj`, `noNewPrivileges`, rutas enmascaradas y de solo lectura, propagación del rootfs y límites de cgroups v2 (`memory.max`, `cpu.max`, `pids.max`). El estado vive en `$XDG_RUNTIME_DIR/anden-runtime/<id>/`, o en `/run/user/<uid>/anden-runtime/<id>/`, o bajo `--root`.

### Decisiones

- Todo el sistema corre sin root dentro de un namespace de usuario: se probó que red, nftables, overlayfs, cgroups delegados y CRIU funcionan así.
- La ruta de despertar `restore` corre sin filtro seccomp, porque CRIU sin root no puede volcar un proceso filtrado.
