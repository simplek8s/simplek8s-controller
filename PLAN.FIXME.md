# PLAN.FIXME — Errores encontrados en PLAN-M1.md

Registro de las discrepancias entre lo que dice `PLAN-M1.md` y el comportamiento
real del API server de Kubernetes, detectadas durante la implementación y la
validación contra un cluster real (minikube, k8s **v1.35.1**, 2 nodos,
2026-09-05). Cada entrada indica el apartado del PLAN afectado, el error, la
evidencia y la corrección aplicada.

---

## 1. El namespace vacío NO acepta creación de recursos namespaced

**Apartados del PLAN afectados:** 3.2 (tabla de verbos), 3.11 (RBAC),
decisión 4 (Eventos), y la nota de `kubectl describe node`.

**Lo que dice el PLAN (original):** el Lease líder
`simplek8s-controller-leader` y los eventos relacionados con Node viven en el
**namespace vacío** (`""`), de modo que `kubectl describe node` los muestra.

**El error:** el API server **rechaza la creación de cualquier recurso
namespaced en el namespace vacío**. Ese namespace es de uso exclusivo del
kubelet (mirror pods de pods estáticos). Verificado en vivo contra v1.35.1
intentando crear `Role`, `RoleBinding`, `ConfigMap`, `Event` y `Lease` en
`/namespaces//...`: todos devuelven

```
422 ... "metadata.namespace: Required value"
```

Además, un `LIST` en `/namespaces//<recurso>` se interpreta como
"todos los namespaces" (devuelve objetos de todos los namespaces), no como el
namespace vacío.

**Corrección aplicada (SUPERADA en parte por la entrada 5):**
- El **Lease** vive en el **namespace del propio controller**
  (`POD_NAMESPACE`, p. ej. `simplek8s`).
- Los **Eventos** de Node: la entrada 5 documenta que el API server también
  rechaza crearlos en el namespace del controller; acaban viviendo en
  `default`.
- `PLAN-M1.md` actualizado en 3.11, la tabla de 3.2 y la decisión 4.

**Nota de despliegue (trampa de kubectl):** `kubectl apply`/`-k` **normaliza**
`namespace: ''` a `default` para recursos namespaced, por lo que un manifiesto
con namespace vacío no puede aplicarse vía kustomize. Al mover el Lease/Events
al namespace del controller este problema desaparece (el `Role` se aplica
normalmente con kustomize).

---

## 2. `Lease.spec.renewTime`/`acquireTime` son `metav1.MicroTime`, no `metav1.Time`

**Apartados del PLAN afectados:** 3.1 / 3.2 (protocolo de Lease, líder).

**Lo que asume el PLAN (implícitamente):** que los campos de tiempo del Lease
usan `metav1.Time` (RFC3339 en segundos, p. ej. `2026-09-05T16:00:00Z`).

**El error:** en el API server real, `Lease.spec.renewTime` y
`Lease.spec.acquireTime` son **`metav1.MicroTime`**, cuyo formato JSON exige
seis dígitos fraccionarios (layout `2006-01-02T15:04:05.000000Z07:00`).
Enviar RFC3339 sin fracción (lo que hacía el cliente) provoca:

```
400 ... parsing time "2026-09-05T16:00:00Z" as
      "2006-01-02T15:04:05.000000Z07:00": cannot parse "Z" as ".000000"
```

Es decir, **toda renovación/adquisición de Lease fallaría** contra un cluster
real (los tests pasaban porque el fake server era permisivo).

**Corrección aplicada:**
- Nuevo tipo `kube.MicroTime` en `internal/kube/types.go` (marshal en el
  layout de 6 dígitos; unmarshal acepta MicroTime, RFC3339Nano y RFC3339).
- `kube.Lease.Spec.RenewTime` ahora es `*MicroTime`.
- `internal/engine/lease.go` crea `kube.MicroTime(now)` para renovar/adquirir.

---

## 3. (Código, no PLAN) `CreateLease` hacía POST a la ruta de ítem

**Nota:** esto no es un error de `PLAN-M1.md`, sino un bug de código detectado en
vivo. Se documenta aquí para trazabilidad.

`Client.CreateLease` hacía `POST /apis/coordination.k8s.io/v1/namespaces/<ns>/leases/<nombre>`
(ruta de ítem). El API server devuelve **`405 Method Not Allowed`** para un
POST a la ruta de ítem; la creación debe ir a la **ruta de colección**
(`.../leases`) y el nombre se toma del cuerpo.

**Corrección aplicada:** `CreateLease` hace POST a la ruta de colección. El
fake (`kubetest`) y el `leaseServer` de los tests de cliente se actualizaron
para extraer el nombre del cuerpo en el POST.

---

## 4. (Código, no PLAN) Sin logs en el controller

**Nota:** no es un error de `PLAN-M1.md`, pero es una carencia detectada en
vivo: el controller no emitía ningún log (el motor solo loguea errores de
API y los eventos fallidos a nivel debug), por lo que no había forma de
verificar que estaba funcionando ni quién era el líder.

**Corrección aplicada:**
- Log de arranque en `main.go` con **versión/commit/fecha de compilación**
  (inyectadas por `-ldflags` desde `Makefile` y `Dockerfile`), node, pod,
  namespace y flags.
- Logs de **transición de estado del Lease** en el engine (solo al cambiar):
  `acquired leadership`, `no valid leader`, `lost leadership`.

---

## 5. Los v1 Event de subjects cluster-scoped (Node) solo se aceptan en el namespace `default`

**Apartados del PLAN afectados:** 3.11 (RBAC), 3.2 (tabla de verbos),
decisión 4 (Eventos).

**Lo que dice el PLAN (y la corrección de la entrada 1):** los eventos
relacionados con Node se crean en el namespace del controller (`simplek8s`),
porque el namespace vacío es de uso exclusivo del kubelet.

**El error (detectado en vivo, cluster SimpleK8s k8s v1.37.0, 2026-09-06):**
el API server también **rechaza** crear un v1 `Event` en el namespace del
controller cuando el `involvedObject` es un subject cluster-scoped (Node,
`involvedObject.namespace` vacío):

```
422 ... Event "reboot-m5vvw" is invalid:
    involvedObject.namespace: Invalid value: "": does not match event.namespace
```

La regla del validador: si `involvedObject.namespace` está vacío (subject
cluster-scoped), el Event debe crearse en el namespace **`default`** — que es
exactamente donde el kubelet publica sus propios eventos de nodo
(`NodeNotSchedulable`, `Starting`, `Rebooted`, ...). Verificado en vivo:
el mismo cuerpo de Event se acepta con `metadata.namespace: "default"` y se
rechaza con `"simplek8s"`.

Consecuencia adicional: los fallos de creación de eventos se registraban
solo a nivel `debug`, por lo que el controller "funcionaba" sin emitir
ningún Event y nadie lo notó hasta el E2E (falta de visibilidad).

**Corrección aplicada:**
- `main.go`: `EventNamespace: "default"` (engine y feature de reboot).
- `deploy/rbac.yaml`: nuevo `Role`/`RoleBinding`
  `simplek8s-controller-events` en el namespace `default` con `events: [create]`.
- El campo muerto `kube.Config.Namespace` se eliminó (se escribía, nunca se
  leía).
- Consulta: `kubectl -n default get events --field-selector
  involvedObject.name=<node>` (actualizado en `README.md`).

## 6. (Diseño, verificado en D5) `NodeDisappeared` puede perderse si el líder muere en la misma ventana

El evento `NodeDisappeared` (3.4) se detecta comparando `prevInflight` — un
mapa **en memoria del proceso líder** con los nodos que estaban
`draining`/`rebooting` en el ciclo anterior — contra el listado actual de
nodos. Si el proceso líder muere entre la desaparición del nodo y el
siguiente ciclo (2 s), la memoria se pierde y el evento nunca se emite.

E2E D5 (2026-09-06): `kubectl delete node wk1` mientras wk1 drenaba, en el
mismo minuto el líder (pod en wk2) reinició con el host (reboot de wk2
admitido tras liberar el slot). Resultado:

- **Correcto**: slot liberado y plan continuó (wk2 `completed`) — el estado
  es anotación-derivative, no hay memoria por nodo que perder.
- **Perdido**: el evento `NodeDisappeared` de wk1 (el nuevo líder no tenía
  `prevInflight`).

No es un bug de correctness: es el límite de observabilidad de un tracking
best-effort en memoria. Alternativas (no aplicadas): persistir
`prevInflight` (p. ej. en el Lease o en una ConfigMap) o emitir el evento
desde la API (DELETE/404) — ambas añaden complejidad para un evento
puramente informativo.

Nota de escape (no es del controller): en esta distribución (k8s 1.37), el
kubelet **no** re-registra un objeto Node borrado en caliente — el nodo
queda fuera del cluster hasta `systemctl restart kubelet` en el host.

---

## 7. (M4, PLAN-M2 3.8/3.10) Notas de implementación del plan de reinicio

No son errores de `PLAN-M2.md`, sino decisiones concretas tomadas al
implementar el plan de reinicio (`full` mode). Se documentan aquí para
trazabilidad.

- **Disparador del plan (3.8).** El plan se dispara *solo* como consecuencia
  de un staging "fresco" en modo `full`. "Fresco" (`freshStage`) significa:
  hay una release disponible, `res.Latest != running`, la release **aún no
  está local** (`!localSet[res.Latest]`) y **no es ya el ancla actual**
  (`ui.NextKernel != res.Latest`). Esto garantiza que un re-staging
  defensivo (los archivos de una versión ya anclada desaparecen de
  `/boot`) **no** re-dispara un plan.
- **Plegado en un solo patch.** Cuando `freshStage && UpdateMode == "full"`,
  la anotación `simplek8s.org/reboot-eligible: <V>` se pliega en el **mismo**
  `PatchTransition` que fija `next-kernel := V` (ancla). El nodo queda así
  "anclado + elegible" atómicamente. En modo `stage` (o en re-staging) se usa
  la ancla sin elegibilidad. El líder es el único que lee
  `reboot-eligible` para formar la membresía del plan.
- **Un plan activo a la vez.** El ConfigMap de estado del plan
  (`simplek8s-update-plans`, clave `plans`, JSON `map[version]PlanEntry`)
  admite un solo plan; el líder no empieza ninguno si ya existe alguno.
  `PlanEntry` = `{startedAt, nodes[], canceling}`.
- **Membresía.** Son miembros los nodos con `reboot-eligible == V` que **no**
  están en quiescencia. Se admite con `EnqueueBuild` (pone
  `reboot-state: requested` y limpia exec/status; **no** emite reboot-request
  directo, de modo que el drenado sigue siendo gobernable por PDB vía M1).
- **Verificación (3.10).** Miembro en `completed` con `running == V` →
  asentado. `completed` con `running != V` → cancela. Miembro `failed` →
  cancela. Reinicio **no** de plan (fuera del plan) en `completed` con
  `next-kernel != running` → reset solo de ese nodo a `running`; un `failed`
  fuera de plan **no** resetea (el nodo queda preparado para el reintento del
  operador).
- **Cancel en dos fases.** Al cancelarse, los miembros **en vuelo**
  (`InFlight`: `draining`/`rebooting`) se **diferen**: no se re-encola ni se
  resetea hasta que su estado M1 aterriza (entonces, si `running == V` se
  mantiene; si no, se resetea a `running`). El plan se borra del ConfigMap
  solo cuando **todos** los miembros están en quiescencia.
- **Tests.** `planstate_test.go` (almacenamiento del plan, reintentos en 409,
  preservación de claves ajenas) y `plan_test.go` (orquestador: arranque +
  admisión, happy-path, mismatch/failed → cancel, takeover, miembro en vuelo
  diferido, verificación no-de-plan, borrado del disparador al asentarse).
  Además, `update_test.go` verifica que solo un staging fresco en `full` fija
  `reboot-eligible` (y que `stage`/re-staging no lo hacen).
