# sdcore-operator blueprint

The operator's install manifests, packaged for Nephio to deploy onto every workload cluster.

## Generate the manifests (from the operator source)
```
cd ../../operator && make manifests
kustomize build config/default > ../blueprints/sdcore-operator/install.yaml
```
`install.yaml` then contains: the `SDCoreDeployment` CRD, RBAC (ServiceAccount/ClusterRole/Binding),
and the controller Deployment (image = `SDCORE_OPERATOR_IMAGE` from versions.env).

The top-level `Makefile` target `blueprint-operator` does this automatically and keeps the image
ref pinned. The workload-cluster-ck8s `pv-sdcore-operator` PackageVariant clones this package into
each cluster's deployment repo; Config Sync applies it.
