# Andén

Plataforma de contenedores con scale-to-zero sobre un datacenter de 3 nodos simulados en una sola máquina. Su eje de diseño es el **presupuesto de arranque en frío**: el tiempo desde la primera petición a un servicio dormido hasta su primer byte de respuesta.

Corre contenedores de proceso con un runtime propio sobre el kernel del host, duerme cualquier servicio TCP sin tráfico y lo despierta de forma transparente reteniendo la primera conexión. Compara dos rutas de despertar (relanzar y restaurar con CRIU) contra runtimes de referencia. Además coloca, despliega sin downtime, revierte y recoloca servicios ante la caída o partición de un nodo.

Cada resultado se mide con un cliente externo y contra una comparación externa, nunca contra una opinión.

## Estado por fase

| Fase | Contenido | Estado |
|---|---|---|
| K0 | Fundación: contrato, investigación y plan | Hecha |
| K1 | Runtime OCI y almacén de imágenes | Pendiente |
| K2 | Datacenter, API y despertar por relanzamiento | Pendiente |
| K3 | Restauración, desglose por fases, descubrimiento y comparación | Pendiente |
| K4 | Colocación, rollouts, caída y partición | Pendiente |
| K5 | Observabilidad, densidad y construcción | Pendiente |
| K6 | Diseño y publicación de resultados | Pendiente |

## Resultados

PENDIENTE: todavía no hay producto ni mediciones.

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
