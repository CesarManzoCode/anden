# AndenWakeOverBudget

El p99 de `anden_wake_duration_seconds` de una ruta pasa de 1 s durante 2 min, sobre una ventana de 5 min. Es el presupuesto de arranque en frío de C8: el tiempo desde que el proxy acepta la primera conexión hasta el primer byte.

## Señales

- `histogram_quantile(0.99, sum by (le, route) (rate(anden_wake_duration_seconds_bucket[5m]))) > 1`.
- Los clientes ven conexiones que tardan en responder después de un periodo de inactividad.
- `anden_wake_phase_seconds{phase, route}` dice qué fase creció: `placement` (plano de control lento o sin respuesta, se espera hasta 200 ms), `rootfs` (overlay o extracción de capas), `network`, `start` (lanzamiento o `criu restore`), `readiness` (el servicio tarda en aceptar) o `first_byte`.

## Diagnóstico

Todos los comandos aceptan `--state DIR`; sin él usan `.run/dc`.

1. Desglose por fase en el agregado:

   ```sh
   anden exec --ns root -- curl -s http://10.80.0.1:9090/metrics
   ```

   Compara `anden_wake_phase_seconds_sum` entre fases de la misma ruta.

2. Despertares individuales: cada línea de `.run/dc/nodes/nodeN/wake_events.jsonl` trae el servicio, la ruta usada, la pedida y las 7 fases en ms. Una línea con `route` distinta de `route_requested` es una restauración que cayó a `relaunch` (D03): el dump o el restore fallaron.

3. Estado de los nodos y de los servicios, para ver si el lento está en un nodo saturado o recién recolocado (un servicio movido pierde su snapshot y despierta por `relaunch`):

   ```sh
   anden status --json
   anden svc list --json
   anden svc get --name SERVICIO --json
   ```

4. Retardo inyectado en el enlace del nodo:

   ```sh
   anden exec --ns root -- tc qdisc show dev veth-node1
   ```

5. Registros del agente del nodo: `.run/dc/nodes/nodeN/agent.log`, con los fallos de `criu` y los reintentos de sondeo.

## Mitigación

- **Retardo de red inyectado**: quítalo.

  ```sh
  anden chaos heal node1
  ```

- **Nodo saturado**: mueve el servicio a otro nodo. Queda dormido allí y despierta por `relaunch` la primera vez:

  ```sh
  anden svc move --name SERVICIO --node node2 --json
  ```

- **Restauraciones que caen a `relaunch` o fase `start` lenta**: despliega el servicio con la otra ruta de despertar, con el resto de la especificación igual:

  ```sh
  anden svc deploy --name SERVICIO --image DIGEST --port PUERTO --cpu-millis C --memory-mib M --wake-route relaunch --json
  ```

- **Servicio que no tolera el arranque en frío**: despliégalo con `--idle-seconds 0` para que no duerma, o despiértalo antes del tráfico:

  ```sh
  anden svc wake --name SERVICIO --json
  ```
