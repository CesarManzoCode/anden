# AndenNodeDown

`anden_node_up{node}` vale 0 durante 30 s: el plano de control no oye la concesión del nodo desde hace más de 4 s (2×TTL, D08). Su agente ya se autofenció: mató sus instancias y cerró las conexiones retenidas.

## Señales

- `anden_node_up{node="nodeN"} == 0` en `10.80.0.1:9090/metrics`.
- `anden status --json` muestra el nodo con `state` distinto de listo.
- Los servicios del nodo cambian de generación y de nodo: el reconciliador los recoloca en los nodos vivos.
- La suma de `anden_wake_*` baja: el agregado deja fuera al agente que no responde.

Caída y partición se ven igual desde el plano de control (D08); el diagnóstico las separa.

## Diagnóstico

Todos los comandos aceptan `--state DIR`; sin él usan `.run/dc`.

1. Estado de los nodos y de los servicios:

   ```sh
   anden status --json
   anden svc list --json
   ```

2. Métricas del agregado y del agente. Si el agente responde en su dirección pero el plano de control no lo oye, es una partición o un plano de control caído:

   ```sh
   anden exec --ns root -- curl -s http://10.80.0.1:9090/metrics
   anden exec --ns node2 -- curl -s http://10.80.0.12:9090/metrics
   ```

3. Enlace del nodo en el netns raíz. `state DOWN` en `veth-nodeN` es una partición; un `qdisc netem` es un retardo inyectado:

   ```sh
   anden exec --ns root -- ip link show veth-node2
   anden exec --ns root -- tc qdisc show dev veth-node2
   ```

4. Registros: `.run/dc/nodes/node2/agent.log` (renovaciones, autofencing) y `.run/dc/andend.log` (nodo declarado muerto, recolocación).

## Mitigación

- **Partición o retardo**: restablece el enlace. El agente vuelve a registrarse con una época nueva y el nodo vuelve a contar como vivo.

  ```sh
  anden chaos heal node2
  ```

- **Caída del agente**: no hay comando para reiniciar un solo agente (ver `LIMITATIONS.md`). Los servicios ya se recolocaron solos; confirma que todos siguen activos o dormidos en otro nodo y que responden:

  ```sh
  anden svc list --json
  anden svc get --name SERVICIO --json
  ```

  Si un servicio quedó sin nodo porque no cabe en los vivos, libera capacidad borrando o durmiendo otros servicios:

  ```sh
  anden svc sleep --name OTRO --json
  ```

- **Datacenter irrecuperable**: el estado deseado vive en Postgres, pero `anden down` borra el estado. Recrea el datacenter y los servicios:

  ```sh
  anden down
  anden up
  ```
