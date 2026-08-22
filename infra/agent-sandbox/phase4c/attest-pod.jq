def expected_tolerations:
  [
    {"effect":"NoExecute","key":"node.kubernetes.io/not-ready","operator":"Exists","tolerationSeconds":300},
    {"effect":"NoExecute","key":"node.kubernetes.io/unreachable","operator":"Exists","tolerationSeconds":300}
  ];
def bounded_runtime_image($expected_image; $expected_digest):
  . == $expected_image or
  test("^sha256:[0-9a-f]{64}$") or
  (test("^[A-Za-z0-9][A-Za-z0-9._/:+-]*@sha256:[0-9a-f]{64}$") and endswith("@" + $expected_digest));
def bound_expected_digest($expected_digest):
  . == $expected_digest or endswith("@" + $expected_digest);

($expected_image | split("@")[1]) as $expected_digest
| if (
    .apiVersion == "v1" and .kind == "Pod" and
    .metadata.name == "phase4c-canary" and .metadata.namespace == "blazn-poc" and
    (.metadata.ownerReferences | length) == 1 and
    .metadata.ownerReferences[0].apiVersion == "agents.x-k8s.io/v1beta1" and
    .metadata.ownerReferences[0].kind == "Sandbox" and
    .metadata.ownerReferences[0].name == "phase4c-canary" and
    .metadata.ownerReferences[0].uid == $sandbox_uid and
    .metadata.ownerReferences[0].controller == true and
    .metadata.ownerReferences[0].blockOwnerDeletion == true and
    .spec.serviceAccountName == "blazn-sandbox-runner" and
    .spec.automountServiceAccountToken == false and
    ((.spec.runtimeClassName // "") == $expected_runtime) and
    .spec.restartPolicy == "Never" and
    .spec.nodeSelector == {"blazn.dev/sandbox-eligible":"true"} and
    ((.spec.nodeName // "") | length) > 0 and
    .spec.schedulerName == "default-scheduler" and
    ((.spec.affinity // {}) | length) == 0 and
    ((.spec.priorityClassName // "") == "") and
    ((.spec.priority // 0) == 0) and
    ((.spec.preemptionPolicy // "PreemptLowerPriority") == "PreemptLowerPriority") and
    ((.spec.tolerations // []) | sort_by(.key)) == expected_tolerations and
    ((.spec.schedulingGates // []) | length) == 0 and
    .spec.securityContext == {"fsGroup":65532,"runAsGroup":65532,"runAsNonRoot":true,"runAsUser":65532,"seccompProfile":{"type":"RuntimeDefault"}} and
    ((.spec.volumes // []) | length) == 0 and
    ((.spec.initContainers // []) | length) == 0 and
    ((.spec.ephemeralContainers // []) | length) == 0 and
    (.spec.containers | length) == 1 and
    .spec.containers[0].name == "main" and
    .spec.containers[0].image == $expected_image and
    .spec.containers[0].command == ["sh","-c","trap : TERM INT; sleep 3600 & wait"] and
    ((.spec.containers[0].args // []) | length) == 0 and
    .spec.containers[0].resources == {"limits":{"cpu":"200m","memory":"128Mi"},"requests":{"cpu":"100m","memory":"64Mi"}} and
    .spec.containers[0].securityContext == {"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"privileged":false,"readOnlyRootFilesystem":true,"runAsNonRoot":true,"seccompProfile":{"type":"RuntimeDefault"}} and
    ((.spec.containers[0].volumeMounts // []) | length) == 0 and
    ((.spec.containers[0].ports // []) | length) == 0 and
    .spec.containers[0].imagePullPolicy == "IfNotPresent" and
    .spec.containers[0].terminationMessagePath == "/dev/termination-log" and
    .spec.containers[0].terminationMessagePolicy == "File" and
    .status.phase == "Running" and
    (.status.containerStatuses | length) == 1 and
    .status.containerStatuses[0].name == "main" and
    (.status.containerStatuses[0].image | bounded_runtime_image($expected_image; $expected_digest)) and
    (.status.containerStatuses[0].imageID | bound_expected_digest($expected_digest)) and
    ((.status.containerStatuses[0] | has("allocatedResources") | not) or .status.containerStatuses[0].allocatedResources == {"cpu":"100m","memory":"64Mi"}) and
    ((.status.containerStatuses[0] | has("resources") | not) or .status.containerStatuses[0].resources == {"limits":{"cpu":"200m","memory":"128Mi"},"requests":{"cpu":"100m","memory":"64Mi"}}) and
    .status.containerStatuses[0].ready == true and
    .status.containerStatuses[0].restartCount == 0 and
    .status.containerStatuses[0].started == true
  ) then
    {
      apiVersion,
      kind,
      metadata: {
        name: .metadata.name,
        namespace: .metadata.namespace,
        uid: .metadata.uid,
        ownerReferences: .metadata.ownerReferences
      },
      spec: {
        serviceAccountName: .spec.serviceAccountName,
        automountServiceAccountToken: .spec.automountServiceAccountToken,
        runtimeClassName: (.spec.runtimeClassName // null),
        restartPolicy: .spec.restartPolicy,
        nodeSelector: .spec.nodeSelector,
        nodeName: .spec.nodeName,
        schedulerName: .spec.schedulerName,
        affinity: (.spec.affinity // null),
        tolerations: (.spec.tolerations // [] | sort_by(.key)),
        priorityClassName: (.spec.priorityClassName // ""),
        priority: (.spec.priority // 0),
        preemptionPolicy: (.spec.preemptionPolicy // "PreemptLowerPriority"),
        schedulingGates: (.spec.schedulingGates // []),
        securityContext: .spec.securityContext,
        volumes: (.spec.volumes // []),
        initContainers: (.spec.initContainers // []),
        containers: [.spec.containers[0] | {
          name, image, command, args: (.args // []), resources, securityContext,
          volumeMounts: (.volumeMounts // []), ports: (.ports // []),
          imagePullPolicy, terminationMessagePath, terminationMessagePolicy
        }],
        ephemeralContainers: (.spec.ephemeralContainers // [])
      },
      status: {
        phase: .status.phase,
        containerStatuses: [.status.containerStatuses[0] | {name,image,imageID,allocatedResources,resources,ready,restartCount,started}]
      }
    }
  else error("generated Phase 4C Pod differs from the closed reviewed runtime contract")
  end
