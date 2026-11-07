# Andén

Plataforma de contenedores con scale-to-zero sobre un datacenter de 3 nodos simulados en una sola máquina. Su eje de diseño es el **presupuesto de arranque en frío**: el tiempo desde la primera petición a un servicio dormido hasta su primer byte de respuesta.

Corre contenedores de proceso con un runtime propio sobre el kernel del host, duerme cualquier servicio TCP sin tráfico y lo despierta de forma transparente reteniendo la primera conexión. Compara dos rutas de despertar (relanzar y restaurar con CRIU) contra runtimes de referencia. Además coloca, despliega sin downtime, revierte y recoloca servicios ante la caída o partición de un nodo.

Cada resultado se mide con un cliente externo y contra una comparación externa, nunca contra una opinión.

## Estado por fase

| Fase | Contenido | Estado |
|---|---|---|
| K0 | Fundación: contrato, investigación y plan | Hecha |
| K1 | Runtime OCI y almacén de imágenes | Cerrada |
| K2 | Datacenter, API y despertar por relanzamiento | Pendiente |
| K3 | Restauración, desglose por fases, descubrimiento y comparación | Pendiente |
| K4 | Colocación, rollouts, caída y partición | Pendiente |
| K5 | Observabilidad, densidad y construcción | Pendiente |
| K6 | Diseño y publicación de resultados | Pendiente |

## Resultados

Medido el 6 nov 2026 en esta máquina (kernel 7.2.14, Ryzen 5 5600G). Los límites de cada cifra están en [`LIMITATIONS.md`](LIMITATIONS.md).

| Criterio | Comparación | Resultado medido |
|---|---|---|
| C3 | Subconjunto OCI de runtime-tools (`bd3b0bd`) | 20 de 20 pruebas pasan contra `anden-runtime`: 2220 aserciones `ok`, 0 `not ok` |
| C6 | `probe` en Andén y en runc 1.5.2 | Informes idénticos: PID 1 propio, hostname propio, procesos del host invisibles, `mount` y `swapon` con `EPERM` |
| C20 | Rootfs de umoci | Iguales en ruta, tipo, modo y hash en alpine (515 entradas), nginx (5014) y la imagen sintética (18) |
| C21 | Digest de cada blob | 7 de 7 blobs coinciden. Con un byte cambiado en una capa, `verify` la nombra y `unpack` sale con 1 |
| C22 | `du -sb` del almacén | nginx y nginx-perl comparten 7 capas, y cada capa se guarda una vez: 76 099 075 bytes, frente a 76 072 090 de capas únicas y 139 984 343 sin compartir |

De K2 en adelante todavía no hay mediciones.

## Uso

Requisitos, comandos y entorno se documentan al existir el producto. El chequeo del repositorio es:

```sh
make check
```

## Documentos

- [`vault/CONTRACT.md`](vault/CONTRACT.md): qué debe hacer el sistema y las decisiones cerradas.
- [`vault/RESEARCH.md`](vault/RESEARCH.md): fuentes y versiones verificadas.
- [`LIMITATIONS.md`](LIMITATIONS.md): lo que el proyecto no demuestra.
- [`CHANGELOG.md`](CHANGELOG.md): historial de cambios.
