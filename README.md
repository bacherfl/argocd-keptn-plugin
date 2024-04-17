<!-- This is an auto-generated file. DO NOT EDIT -->
# keptn

* Needs: 
* Image: bacherfl/keptn-argo-executor



Install:

    kubectl apply -f config/rbac.yaml
    kubectl apply -f config/keptn-executor-plugin-configmap.yaml

Uninstall:
	
    kubectl delete cm keptn-executor-plugin 
