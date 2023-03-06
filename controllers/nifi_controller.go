/*
Copyright 2023.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	//"os"
	"strconv"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	bigdatav1alpha1 "github.com/kubernetesbigdataeg/nifi-operator/api/v1alpha1"
)

// Definitions to manage status conditions
const (
	nifiFinalizer = "bigdata.kubernetesbigdataeg.org/finalizer"
	// typeAvailableNifi represents the status of the Deployment reconciliation
	typeAvailableNifi = "Available"
	// typeDegradedNifi represents the status used when the custom resource is deleted and the finalizer operations are must to occur.
	typeDegradedNifi = "Degraded"
)

// NifiReconciler reconciles a Nifi object
type NifiReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// The following markers are used to generate the rules permissions (RBAC) on config/rbac using controller-gen
// when the command <make manifests> is executed.
// To know more about markers see: https://book.kubebuilder.io/reference/markers.html

//+kubebuilder:rbac:groups=bigdata.kubernetesbigdataeg.org,resources=nifis,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=bigdata.kubernetesbigdataeg.org,resources=nifis/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=bigdata.kubernetesbigdataeg.org,resources=nifis/finalizers,verbs=update
//+kubebuilder:rbac:groups=core,resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=configmaps;services,verbs=get;list;create;watch
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.

// It is essential for the controller's reconciliation loop to be idempotent. By following the Operator
// pattern you will create Controllers which provide a reconcile function
// responsible for synchronizing resources until the desired state is reached on the cluster.
// Breaking this recommendation goes against the design principles of controller-runtime.
// and may lead to unforeseen consequences such as resources becoming stuck and requiring manual intervention.
// For further info:
// - About Operator Pattern: https://kubernetes.io/docs/concepts/extend-kubernetes/operator/
// - About Controllers: https://kubernetes.io/docs/concepts/architecture/controller/
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *NifiReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	//
	// 1. Control-loop: checking if Nifi CR exists
	//
	// The purpose is check if the Custom Resource for the Kind Nifi
	// is applied on the cluster if not we return nil to stop the reconciliation
	nifi := &bigdatav1alpha1.Nifi{}
	err := r.Get(ctx, req.NamespacedName, nifi)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If the custom resource is not found then, it usually means that it was deleted or not created
			// In this way, we will stop the reconciliation
			log.Info("nifi resource not found. Ignoring since object must be deleted")
			return ctrl.Result{}, nil
		}
		// Error reading the object - requeue the request.
		log.Error(err, "Failed to get nifi")
		return ctrl.Result{}, err
	}

	//
	// 2. Control-loop: Status to Unknown
	//
	// Let's just set the status as Unknown when no status are available
	if nifi.Status.Conditions == nil || len(nifi.Status.Conditions) == 0 {
		meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeAvailableNifi, Status: metav1.ConditionUnknown, Reason: "Reconciling", Message: "Starting reconciliation"})
		if err = r.Status().Update(ctx, nifi); err != nil {
			log.Error(err, "Failed to update Nifi status")
			return ctrl.Result{}, err
		}

		// Let's re-fetch the nifi Custom Resource after update the status
		// so that we have the latest state of the resource on the cluster and we will avoid
		// raise the issue "the object has been modified, please apply
		// your changes to the latest version and try again" which would re-trigger the reconciliation
		// if we try to update it again in the following operations
		if err := r.Get(ctx, req.NamespacedName, nifi); err != nil {
			log.Error(err, "Failed to re-fetch nifi")
			return ctrl.Result{}, err
		}
	}

	//
	// 3. Control-loop: Let's add a finalizer
	//
	// Let's add a finalizer. Then, we can define some operations which should
	// occurs before the custom resource to be deleted.
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/finalizers
	if !controllerutil.ContainsFinalizer(nifi, nifiFinalizer) {
		log.Info("Adding Finalizer for Nifi")
		if ok := controllerutil.AddFinalizer(nifi, nifiFinalizer); !ok {
			log.Error(err, "Failed to add finalizer into the custom resource")
			return ctrl.Result{Requeue: true}, nil
		}

		if err = r.Update(ctx, nifi); err != nil {
			log.Error(err, "Failed to update custom resource to add finalizer")
			return ctrl.Result{}, err
		}
	}

	//
	// 4. Control-loop: Instance marked for deletion
	//
	// Check if the Nifi instance is marked to be deleted, which is
	// indicated by the deletion timestamp being set.
	isNifiMarkedToBeDeleted := nifi.GetDeletionTimestamp() != nil
	if isNifiMarkedToBeDeleted {
		if controllerutil.ContainsFinalizer(nifi, nifiFinalizer) {
			log.Info("Performing Finalizer Operations for Nifi before delete CR")

			// Let's add here an status "Downgrade" to define that this resource begin its process to be terminated.
			meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeDegradedNifi,
				Status: metav1.ConditionUnknown, Reason: "Finalizing",
				Message: fmt.Sprintf("Performing finalizer operations for the custom resource: %s ", nifi.Name)})

			if err := r.Status().Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to update Nifi status")
				return ctrl.Result{}, err
			}

			// Perform all operations required before remove the finalizer and allow
			// the Kubernetes API to remove the custom resource.
			r.doFinalizerOperationsForNifi(nifi)

			// TODO(user): If you add operations to the doFinalizerOperationsForNifi method
			// then you need to ensure that all worked fine before deleting and updating the Downgrade status
			// otherwise, you should requeue here.

			// Re-fetch the nifi Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, nifi); err != nil {
				log.Error(err, "Failed to re-fetch nifi")
				return ctrl.Result{}, err
			}

			meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeDegradedNifi,
				Status: metav1.ConditionTrue, Reason: "Finalizing",
				Message: fmt.Sprintf("Finalizer operations for custom resource %s name were successfully accomplished", nifi.Name)})

			if err := r.Status().Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to update Nifi status")
				return ctrl.Result{}, err
			}

			log.Info("Removing Finalizer for Nifi after successfully perform the operations")
			if ok := controllerutil.RemoveFinalizer(nifi, nifiFinalizer); !ok {
				log.Error(err, "Failed to remove finalizer for Nifi")
				return ctrl.Result{Requeue: true}, nil
			}

			if err := r.Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to remove finalizer for Nifi")
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	//
	// 5. Control-loop: Let's deploy/ensure our managed resources for Nifi
	// - ConfigMap,
	// - Service Headless,
	// - Service ClusterIP,
	// - StateFulSet
	//

	// ConfigMap: Check if the ConfigMap already exists, if not create a new one
	configMapFound := &corev1.ConfigMap{}
	err = r.Get(ctx, types.NamespacedName{Name: "nifi-config", Namespace: nifi.Namespace}, configMapFound)
	if err != nil && apierrors.IsNotFound(err) {
		// Define the default ConfigMap
		cm, err := r.defaultConfigMapForNifi(nifi)
		if err != nil {
			log.Error(err, "Failed to define new ConfigMap resource for Nifi")

			// The following implementation will update the status
			meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeAvailableNifi,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create ConfigMap for the custom resource (%s): (%s)",
					nifi.Name, err)})

			if err := r.Status().Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to update Nifi status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new ConfigMap",
			"ConfigMap.Namespace", cm.Namespace, "ConfigMap.Name", cm.Name)

		if err = r.Create(ctx, cm); err != nil {
			log.Error(err, "Failed to create new ConfigMap",
				"ConfigMap.Namespace", cm.Namespace, "ConfigMap.Name", cm.Name)
			return ctrl.Result{}, err
		}

		// ConfigMap created successfully at this point.
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		//return ctrl.Result{RequeueAfter: time.Minute}, nil

	} else if err != nil {
		log.Error(err, "Failed to get ConfigMap")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// Service Headless: Check if the headless svc already exists, if not create a new one
	serviceHeadlessFound := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: "nifi-hs", Namespace: nifi.Namespace}, serviceHeadlessFound)
	if err != nil && apierrors.IsNotFound(err) {

		hsvc, err := r.serviceHeadlessForNifi(nifi)
		if err != nil {
			log.Error(err, "Failed to define new Headless Service resource for Nifi")

			// The following implementation will update the status
			meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeAvailableNifi,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Headless Service for the custom resource (%s): (%s)",
					nifi.Name, err)})

			if err := r.Status().Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to update Nifi status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Headless Service",
			"Service.Namespace", hsvc.Namespace, "Service.Name", hsvc.Name)

		if err = r.Create(ctx, hsvc); err != nil {
			log.Error(err, "Failed to create new Headless Service",
				"Service.Namespace", hsvc.Namespace, "Service.Name", hsvc.Name)
			return ctrl.Result{}, err
		}

		// Service created successfully at this point.
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		//return ctrl.Result{RequeueAfter: time.Minute}, nil

	} else if err != nil {
		log.Error(err, "Failed to get Headless Service")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// Service: Check if the headless svc already exists, if not create a new one
	serviceFound := &corev1.Service{}
	err = r.Get(ctx, types.NamespacedName{Name: "nifi-svc", Namespace: nifi.Namespace}, serviceFound)
	if err != nil && apierrors.IsNotFound(err) {

		svc, err := r.serviceForNifi(nifi)
		if err != nil {
			log.Error(err, "Failed to define new Service resource for Nifi")

			// The following implementation will update the status
			meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeAvailableNifi,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create Service for the custom resource (%s): (%s)",
					nifi.Name, err)})

			if err := r.Status().Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to update Nifi status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new Service",
			"Service.Namespace", svc.Namespace, "Service.Name", svc.Name)

		if err = r.Create(ctx, svc); err != nil {
			log.Error(err, "Failed to create new Service",
				"Service.Namespace", svc.Namespace, "Service.Name", svc.Name)
			return ctrl.Result{}, err
		}

		// Service created successfully at this point.
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		//return ctrl.Result{RequeueAfter: time.Minute}, nil

	} else if err != nil {
		log.Error(err, "Failed to get Headless Service")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	// StateFulSet: Check if the sts already exists, if not create a new one
	found := &appsv1.StatefulSet{}
	err = r.Get(ctx, types.NamespacedName{Name: nifi.Name, Namespace: nifi.Namespace}, found)
	if err != nil && apierrors.IsNotFound(err) {
		// Define a new sts
		dep, err := r.stateFulSetForNifi(nifi)
		if err != nil {
			log.Error(err, "Failed to define new StateFulSet resource for Nifi")

			// The following implementation will update the status
			meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeAvailableNifi,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create StatefulSet for the custom resource (%s): (%s)", nifi.Name, err)})

			if err := r.Status().Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to update Nifi status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		log.Info("Creating a new StateFulSet",
			"StatefulSet.Namespace", dep.Namespace, "StatefulSet.Name", dep.Name)

		if err = r.Create(ctx, dep); err != nil {
			log.Error(err, "Failed to create new StatefulSet",
				"StatefulSet.Namespace", dep.Namespace, "StatefulSet.Name", dep.Name)
			return ctrl.Result{}, err
		}

		// StatefulSet created successfully at this point.
		// We will requeue the reconciliation so that we can ensure the state
		// and move forward for the next operations
		return ctrl.Result{RequeueAfter: time.Minute}, nil
	} else if err != nil {
		log.Error(err, "Failed to get StatefulSet")
		// Let's return the error for the reconciliation be re-trigged again
		return ctrl.Result{}, err
	}

	//
	// 6. Control-loop: Check the number of replicas
	//
	// The CRD API is defining that the Nifi type, have a NifiSpec.Size field
	// to set the quantity of Deployment instances is the desired state on the cluster.
	// Therefore, the following code will ensure the Deployment size is the same as defined
	// via the Size spec of the Custom Resource which we are reconciling.
	size := nifi.Spec.Size
	if *found.Spec.Replicas != size {
		found.Spec.Replicas = &size
		if err = r.Update(ctx, found); err != nil {
			log.Error(err, "Failed to update StatefulSet",
				"StatefulSet.Namespace", found.Namespace, "StatefulSet.Name", found.Name)

			// Re-fetch the nifi Custom Resource before update the status
			// so that we have the latest state of the resource on the cluster and we will avoid
			// raise the issue "the object has been modified, please apply
			// your changes to the latest version and try again" which would re-trigger the reconciliation
			if err := r.Get(ctx, req.NamespacedName, nifi); err != nil {
				log.Error(err, "Failed to re-fetch nifi")
				return ctrl.Result{}, err
			}

			// The following implementation will update the status
			meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeAvailableNifi,
				Status: metav1.ConditionFalse, Reason: "Resizing",
				Message: fmt.Sprintf("Failed to update the size for the custom resource (%s): (%s)", nifi.Name, err)})

			if err := r.Status().Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to update Nifi status")
				return ctrl.Result{}, err
			}

			return ctrl.Result{}, err
		}

		// Now, that we update the size we want to requeue the reconciliation
		// so that we can ensure that we have the latest state of the resource before
		// update. Also, it will help ensure the desired state on the cluster
		return ctrl.Result{Requeue: true}, nil
	}

	//
	// 7. Control-loop: Let's update the status
	//
	// The following implementation will update the status
	meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeAvailableNifi,
		Status: metav1.ConditionTrue, Reason: "Reconciling",
		Message: fmt.Sprintf("Deployment for custom resource (%s) with %d replicas created successfully", nifi.Name, size)})

	if err := r.Status().Update(ctx, nifi); err != nil {
		log.Error(err, "Failed to update Nifi status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
} // end control-loop function

// finalizeNifi will perform the required operations before delete the CR.
func (r *NifiReconciler) doFinalizerOperationsForNifi(cr *bigdatav1alpha1.Nifi) {
	// TODO(user): Add the cleanup steps that the operator
	// needs to do before the CR can be deleted. Examples
	// of finalizers include performing backups and deleting
	// resources that are not owned by this CR, like a PVC.

	// Note: It is not recommended to use finalizers with the purpose of delete resources which are
	// created and managed in the reconciliation. These ones, such as the Deployment created on this reconcile,
	// are defined as depended of the custom resource. See that we use the method ctrl.SetControllerReference.
	// to set the ownerRef which means that the Deployment will be deleted by the Kubernetes API.
	// More info: https://kubernetes.io/docs/tasks/administer-cluster/use-cascading-deletion/

	// The following implementation will raise an event
	r.Recorder.Event(cr, "Warning", "Deleting",
		fmt.Sprintf("Custom Resource %s is being deleted from the namespace %s",
			cr.Name,
			cr.Namespace))
}

func (r *NifiReconciler) defaultConfigMapForNifi(
	v *bigdatav1alpha1.Nifi) (*corev1.ConfigMap, error) {

	namespace := v.Namespace
	hostname := v.Name

	configMapData := make(map[string]string, 0)
	nifiEnvPartial := `
    export NIFI__nifiproperties__SINGLE_USER_CREDENTIALS_USERNAME="admin"
    export NIFI__nifiproperties__SINGLE_USER_CREDENTIALS_PASSWORD="admin123456789"
    export NIFI__nifiproperties__NIFI_SENSITIVE_PROPS_KEY="randomstring12charsmin"
    export NIFI__nifiproperties__NIFI_WEB_HTTP_PORT="8080"
    export NIFI__nifiproperties__NIFI_CLUSTER_IS_NODE="true"
    export NIFI__nifiproperties__NIFI_CLUSTER_NODE_PROTOCOL_PORT="8082"
    export NIFI__nifiproperties__NIFI_CLUSTER_NODE_PROTOCOL_MAX_THREADS="20"
    export NIFI__nifiproperties__NIFI_ANALYTICS_PREDICT_ENABLED="true"
    export NIFI__nifiproperties__NIFI_ELECTION_MAX_CANDIDATES="1"
    export NIFI__nifiproperties__IS_CLUSTER_NODE="yes"
    export NIFI__nifiproperties__ZK_MONITOR_PORT="2888"
    export NIFI__nifiproperties__NIFI_ELECTION_MAX_WAIT="20 sec"
    export NIFI__nifiproperties__NIFI_JVM_HEAP_INIT="3g"
    export NIFI__nifiproperties__NIFI_JVM_HEAP_MAX="4g"
    export NIFI__nifiproperties__NIFI_FLOW_CONFIGURATION_FILE="./conf/flow.xml.gz"
    export NIFI__nifiproperties__NIFI_FLOW_CONFIGURATION_JSON_FILE="./conf/flow.json.gz"
    export NIFI__nifiproperties__NIFI_FLOW_CONFIGURATION_ARCHIVE_ENABLED="true"
    export NIFI__nifiproperties__NIFI_FLOW_CONFIGURATION_ARCHIVE_DIR="./conf/archive/"
    export NIFI__nifiproperties__NIFI_FLOW_CONFIGURATION_ARCHIVE_MAX_TIME="30 days"
    export NIFI__nifiproperties__NIFI_FLOW_CONFIGURATION_ARCHIVE_MAX_STORAGE="500 MB"
    export NIFI__nifiproperties__NIFI_FLOW_CONFIGURATION_ARCHIVE_MAX_COUNT=""
    export NIFI__nifiproperties__NIFI_FLOWCONTROLLER_AUTORESUMESTATE="true"
    export NIFI__nifiproperties__NIFI_FLOWCONTROLLER_GRACEFUL_SHUTDOWN_PERIOD="10 sec"
    export NIFI__nifiproperties__NIFI_FLOWSERVICE_WRITEDELAY_INTERVAL="500 ms"
    export NIFI__nifiproperties__NIFI_ADMINISTRATIVE_YIELD_DURATION="30 sec"
    export NIFI__nifiproperties__NIFI_BORED_YIELD_DURATION="10 millis"
    export NIFI__nifiproperties__NIFI_QUEUE_BACKPRESSURE_COUNT="10000"
    export NIFI__nifiproperties__NIFI_QUEUE_BACKPRESSURE_SIZE="1 GB"
    export NIFI__nifiproperties__NIFI_AUTHORIZER_CONFIGURATION_FILE="./conf/authorizers.xml"
    export NIFI__nifiproperties__NIFI_LOGIN_IDENTITY_PROVIDER_CONFIGURATION_FILE="./conf/login-identity-providers.xml"
    export NIFI__nifiproperties__NIFI_TEMPLATES_DIRECTORY="./conf/templates"
    export NIFI__nifiproperties__NIFI_UI_BANNER_TEXT=""
    export NIFI__nifiproperties__NIFI_UI_AUTOREFRESH_INTERVAL="30 sec"
    export NIFI__nifiproperties__NIFI_NAR_LIBRARY_DIRECTORY="./lib"
    export NIFI__nifiproperties__NIFI_NAR_LIBRARY_AUTOLOAD_DIRECTORY="./extensions"
    export NIFI__nifiproperties__NIFI_NAR_WORKING_DIRECTORY="./work/nar/"
    export NIFI__nifiproperties__NIFI_DOCUMENTATION_WORKING_DIRECTORY="./work/docs/components"
    export NIFI__nifiproperties__NIFI_STATE_MANAGEMENT_CONFIGURATION_FILE="./conf/state-management.xml"
    export NIFI__nifiproperties__NIFI_STATE_MANAGEMENT_PROVIDER_LOCAL="local-provider"
    export NIFI__nifiproperties__NIFI_STATE_MANAGEMENT_PROVIDER_CLUSTER="zk-provider"
    export NIFI__nifiproperties__NIFI_STATE_MANAGEMENT_EMBEDDED_ZOOKEEPER_START="false"
    export NIFI__nifiproperties__NIFI_STATE_MANAGEMENT_EMBEDDED_ZOOKEEPER_PROPERTIES="./conf/zookeeper.properties"
    export NIFI__nifiproperties__NIFI_DATABASE_DIRECTORY="./database_repository"
    export NIFI__nifiproperties__NIFI_H2_URL_APPEND=";LOCK_TIMEOUT=25000;WRITE_DELAY=0;AUTO_SERVER=FALSE"
    export NIFI__nifiproperties__NIFI_REPOSITORY_ENCRYPTION_PROTOCOL_VERSION=""
    export NIFI__nifiproperties__NIFI_REPOSITORY_ENCRYPTION_KEY_ID=""
    export NIFI__nifiproperties__NIFI_REPOSITORY_ENCRYPTION_KEY_PROVIDER=""
    export NIFI__nifiproperties__NIFI_REPOSITORY_ENCRYPTION_KEY_PROVIDER_KEYSTORE_LOCATION=""
    export NIFI__nifiproperties__NIFI_REPOSITORY_ENCRYPTION_KEY_PROVIDER_KEYSTORE_PASSWORD=""
    export NIFI__nifiproperties__NIFI_FLOWFILE_REPOSITORY_IMPLEMENTATION="org.apache.nifi.controller.repository.WriteAheadFlowFileRepository"
    export NIFI__nifiproperties__NIFI_FLOWFILE_REPOSITORY_WAL_IMPLEMENTATION="org.apache.nifi.wali.SequentialAccessWriteAheadLog"
    export NIFI__nifiproperties__NIFI_FLOWFILE_REPOSITORY_DIRECTORY="./flowfile_repository"
    export NIFI__nifiproperties__NIFI_FLOWFILE_REPOSITORY_CHECKPOINT_INTERVAL="20 secs"
    export NIFI__nifiproperties__NIFI_FLOWFILE_REPOSITORY_ALWAYS_SYNC="false"
    export NIFI__nifiproperties__NIFI_FLOWFILE_REPOSITORY_RETAIN_ORPHANED_FLOWFILES="true"
    export NIFI__nifiproperties__NIFI_SWAP_MANAGER_IMPLEMENTATION="org.apache.nifi.controller.FileSystemSwapManager"
    export NIFI__nifiproperties__NIFI_QUEUE_SWAP_THRESHOLD="20000"
    export NIFI__nifiproperties__NIFI_CONTENT_REPOSITORY_IMPLEMENTATION="org.apache.nifi.controller.repository.FileSystemRepository"
    export NIFI__nifiproperties__NIFI_CONTENT_CLAIM_MAX_APPENDABLE_SIZE="50 KB"
    export NIFI__nifiproperties__NIFI_CONTENT_REPOSITORY_DIRECTORY_DEFAULT="./content_repository"
    export NIFI__nifiproperties__NIFI_CONTENT_REPOSITORY_ARCHIVE_MAX_RETENTION_PERIOD="7 days"
    export NIFI__nifiproperties__NIFI_CONTENT_REPOSITORY_ARCHIVE_MAX_USAGE_PERCENTAGE="50%"
    export NIFI__nifiproperties__NIFI_CONTENT_REPOSITORY_ARCHIVE_ENABLED="true"
    export NIFI__nifiproperties__NIFI_CONTENT_REPOSITORY_ALWAYS_SYNC="false"
    export NIFI__nifiproperties__NIFI_CONTENT_VIEWER_URL="../nifi-content-viewer/"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_IMPLEMENTATION="org.apache.nifi.provenance.WriteAheadProvenanceRepository"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_DIRECTORY_DEFAULT="./provenance_repository"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_MAX_STORAGE_TIME="30 days"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_MAX_STORAGE_SIZE="10 GB"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_ROLLOVER_TIME="10 mins"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_ROLLOVER_SIZE="100 MB"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_QUERY_THREADS="2"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_INDEX_THREADS="2"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_COMPRESS_ON_ROLLOVER="true"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_ALWAYS_SYNC="falsenifi_provenance_repository_indexed_fields=EventType, FlowFileUUID, Filename, ProcessorID, Relationship"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_INDEXED_ATTRIBUTES=""
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_INDEX_SHARD_SIZE="500 MB"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_MAX_ATTRIBUTE_LENGTH="65536"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_CONCURRENT_MERGE_THREADS="2"
    export NIFI__nifiproperties__NIFI_PROVENANCE_REPOSITORY_BUFFER_SIZE="100000"
    export NIFI__nifiproperties__NIFI_COMPONENTS_STATUS_REPOSITORY_IMPLEMENTATION="org.apache.nifi.controller.status.history.VolatileComponentStatusRepository"
    export NIFI__nifiproperties__NIFI_COMPONENTS_STATUS_REPOSITORY_BUFFER_SIZE="1440"
    export NIFI__nifiproperties__NIFI_COMPONENTS_STATUS_SNAPSHOT_FREQUENCY="1 min"
    export NIFI__nifiproperties__NIFI_STATUS_REPOSITORY_QUESTDB_PERSIST_NODE_DAYS="14"
    export NIFI__nifiproperties__NIFI_STATUS_REPOSITORY_QUESTDB_PERSIST_COMPONENT_DAYS="3"
    export NIFI__nifiproperties__NIFI_STATUS_REPOSITORY_QUESTDB_PERSIST_LOCATION="./status_repository"
    export NIFI__nifiproperties__NIFI_REMOTE_INPUT_HOST="nifi-0"
    export NIFI__nifiproperties__NIFI_REMOTE_INPUT_SECURE="false"
    export NIFI__nifiproperties__NIFI_REMOTE_INPUT_SOCKET_PORT="10000"
    export NIFI__nifiproperties__NIFI_REMOTE_INPUT_HTTP_ENABLED="true"
    export NIFI__nifiproperties__NIFI_REMOTE_INPUT_HTTP_TRANSACTION_TTL="30 sec"
    export NIFI__nifiproperties__NIFI_REMOTE_CONTENTS_CACHE_EXPIRATION="30 secs"
    export NIFI__nifiproperties__NIFI_WEB_HTTP_NETWORK_INTERFACE_DEFAULT=""
    export NIFI__nifiproperties__NIFI_WEB_HTTPS_HOST=""
    export NIFI__nifiproperties__NIFI_WEB_HTTPS_PORT=""
    export NIFI__nifiproperties__NIFI_WEB_HTTPS_NETWORK_INTERFACE_DEFAULT=""
    export NIFI__nifiproperties__NIFI_WEB_JETTY_WORKING_DIRECTORY="./work/jetty"
    export NIFI__nifiproperties__NIFI_WEB_JETTY_THREADS="200"
    export NIFI__nifiproperties__NIFI_WEB_MAX_HEADER_SIZE="16 KB"
    export NIFI__nifiproperties__NIFI_WEB_PROXY_CONTEXT_PATH=""
    export NIFI__nifiproperties__NIFI_WEB_PROXY_HOST=""
    export NIFI__nifiproperties__NIFI_WEB_MAX_CONTENT_SIZE=""
    export NIFI__nifiproperties__NIFI_WEB_MAX_REQUESTS_PER_SECOND="30000"
    export NIFI__nifiproperties__NIFI_WEB_MAX_ACCESS_TOKEN_REQUESTS_PER_SECOND="25"
    export NIFI__nifiproperties__NIFI_WEB_REQUEST_TIMEOUT="60 secs"
    export NIFI__nifiproperties__NIFI_WEB_REQUEST_IP_WHITELIST=""
    export NIFI__nifiproperties__NIFI_WEB_SHOULD_SEND_SERVER_VERSION="true"
    export NIFI__nifiproperties__NIFI_WEB_REQUEST_LOG_FORMAT="""%{client}a - %u %t "%r" %s %O "%{Referer}i" "%{User-Agent}i""""
    export NIFI__nifiproperties__NIFI_WEB_HTTPS_CIPHERSUITES_INCLUDE=""
    export NIFI__nifiproperties__NIFI_WEB_HTTPS_CIPHERSUITES_EXCLUDE=""
    export NIFI__nifiproperties__NIFI_SENSITIVE_PROPS_KEY_PROTECTED=""
    export NIFI__nifiproperties__NIFI_SENSITIVE_PROPS_ALGORITHM="NIFI_PBKDF2_AES_GCM_256"
    export NIFI__nifiproperties__NIFI_SENSITIVE_PROPS_ADDITIONAL_KEYS=""
    export NIFI__nifiproperties__NIFI_SECURITY_AUTORELOAD_ENABLED="false"
    export NIFI__nifiproperties__NIFI_SECURITY_AUTORELOAD_INTERVAL="10 secs"
    export NIFI__nifiproperties__NIFI_SECURITY_KEYSTORE=""
    export NIFI__nifiproperties__NIFI_SECURITY_KEYSTORETYPE=""
    export NIFI__nifiproperties__NIFI_SECURITY_KEYSTOREPASSWD=""
    export NIFI__nifiproperties__NIFI_SECURITY_KEYPASSWD=""
    export NIFI__nifiproperties__NIFI_SECURITY_TRUSTSTORE=""
    export NIFI__nifiproperties__NIFI_SECURITY_TRUSTSTORETYPE=""
    export NIFI__nifiproperties__NIFI_SECURITY_TRUSTSTOREPASSWD=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_AUTHORIZER="single-user-authorizer"
    export NIFI__nifiproperties__NIFI_SECURITY_ALLOW_ANONYMOUS_AUTHENTICATION="false"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_LOGIN_IDENTITY_PROVIDER=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_JWS_KEY_ROTATION_PERIOD="PT1H"
    export NIFI__nifiproperties__NIFI_SECURITY_OCSP_RESPONDER_URL=""
    export NIFI__nifiproperties__NIFI_SECURITY_OCSP_RESPONDER_CERTIFICATE=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_DISCOVERY_URL=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_CONNECT_TIMEOUT="5 secs"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_READ_TIMEOUT="5 secs"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_CLIENT_ID=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_CLIENT_SECRET=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_PREFERRED_JWSALGORITHM=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_ADDITIONAL_SCOPES=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_CLAIM_IDENTIFYING_USER=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_FALLBACK_CLAIMS_IDENTIFYING_USER=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_OIDC_TRUSTSTORE_STRATEGY="JDK"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_KNOX_URL=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_KNOX_PUBLICKEY=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_KNOX_COOKIENAME="hadoop-jwt"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_KNOX_AUDIENCES=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_IDP_METADATA_URL=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_SP_ENTITY_ID=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_IDENTITY_ATTRIBUTE_NAME=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_GROUP_ATTRIBUTE_NAME=""
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_METADATA_SIGNING_ENABLED="false"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_REQUEST_SIGNING_ENABLED="false"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_SIGNATURE_ALGORITHM="http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_SIGNATURE_DIGEST_ALGORITHM="http://www.w3.org/2001/04/xmlenc#sha256"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_MESSAGE_LOGGING_ENABLED="false"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_AUTHENTICATION_EXPIRATION="12 hours"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_SINGLE_LOGOUT_ENABLED="false"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_HTTP_CLIENT_TRUSTSTORE_STRATEGY="JDK"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_HTTP_CLIENT_CONNECT_TIMEOUT="30 secs"
    export NIFI__nifiproperties__NIFI_SECURITY_USER_SAML_HTTP_CLIENT_READ_TIMEOUT="30 secs"
    export NIFI__nifiproperties__NIFI_LISTENER_BOOTSTRAP_PORT="0"
    export NIFI__nifiproperties__NIFI_CLUSTER_PROTOCOL_HEARTBEAT_INTERVAL="5 sec"
    export NIFI__nifiproperties__NIFI_CLUSTER_PROTOCOL_HEARTBEAT_MISSABLE_MAX="8"
    export NIFI__nifiproperties__NIFI_CLUSTER_PROTOCOL_IS_SECURE="false"
    export NIFI__nifiproperties__NIFI_CLUSTER_NODE_ADDRESS="nifi-0.nifi-hs.default.svc.cluster.local"
    export NIFI__nifiproperties__NIFI_CLUSTER_NODE_EVENT_HISTORY_SIZE="25"
    export NIFI__nifiproperties__NIFI_CLUSTER_NODE_CONNECTION_TIMEOUT="5 sec"
    export NIFI__nifiproperties__NIFI_CLUSTER_NODE_READ_TIMEOUT="5 sec"
    export NIFI__nifiproperties__NIFI_CLUSTER_NODE_MAX_CONCURRENT_REQUESTS="100"
    export NIFI__nifiproperties__NIFI_CLUSTER_FIREWALL_FILE=""
    export NIFI__nifiproperties__NIFI_CLUSTER_FLOW_ELECTION_MAX_WAIT_TIME="20 sec"
    export NIFI__nifiproperties__NIFI_CLUSTER_FLOW_ELECTION_MAX_CANDIDATES="1"
    export NIFI__nifiproperties__NIFI_CLUSTER_LOAD_BALANCE_HOST=""
    export NIFI__nifiproperties__NIFI_CLUSTER_LOAD_BALANCE_PORT="6342"
    export NIFI__nifiproperties__NIFI_CLUSTER_LOAD_BALANCE_CONNECTIONS_PER_NODE="1"
    export NIFI__nifiproperties__NIFI_CLUSTER_LOAD_BALANCE_MAX_THREAD_COUNT="8"
    export NIFI__nifiproperties__NIFI_CLUSTER_LOAD_BALANCE_COMMS_TIMEOUT="30 sec"
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_CONNECT_TIMEOUT="10 secs"
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_SESSION_TIMEOUT="10 secs"
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_ROOT_NODE="/nifi"
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_CLIENT_SECURE="false"
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_SECURITY_KEYSTORE=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_SECURITY_KEYSTORETYPE=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_SECURITY_KEYSTOREPASSWD=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_SECURITY_TRUSTSTORE=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_SECURITY_TRUSTSTORETYPE=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_SECURITY_TRUSTSTOREPASSWD=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_JUTE_MAXBUFFER=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_AUTH_TYPE=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_KERBEROS_REMOVEHOSTFROMPRINCIPAL=""
    export NIFI__nifiproperties__NIFI_ZOOKEEPER_KERBEROS_REMOVEREALMFROMPRINCIPAL=""
    export NIFI__nifiproperties__NIFI_KERBEROS_KRB5_FILE=""
    export NIFI__nifiproperties__NIFI_KERBEROS_SERVICE_PRINCIPAL=""
    export NIFI__nifiproperties__NIFI_KERBEROS_SERVICE_KEYTAB_LOCATION=""
    export NIFI__nifiproperties__NIFI_KERBEROS_SPNEGO_PRINCIPAL=""
    export NIFI__nifiproperties__NIFI_KERBEROS_SPNEGO_KEYTAB_LOCATION=""
    export NIFI__nifiproperties__NIFI_KERBEROS_SPNEGO_AUTHENTICATION_EXPIRATION="12 hours"
    export NIFI__nifiproperties__NIFI_VARIABLE_REGISTRY_PROPERTIES=""
    export NIFI__nifiproperties__NIFI_ANALYTICS_PREDICT_INTERVAL="3 mins"
    export NIFI__nifiproperties__NIFI_ANALYTICS_QUERY_INTERVAL="5 mins"
    export NIFI__nifiproperties__NIFI_ANALYTICS_CONNECTION_MODEL_IMPLEMENTATION="org.apache.nifi.controller.status.analytics.models.OrdinaryLeastSquares"
    export NIFI__nifiproperties__NIFI_ANALYTICS_CONNECTION_MODEL_SCORE_NAME="rSquared"
    export NIFI__nifiproperties__NIFI_ANALYTICS_CONNECTION_MODEL_SCORE_THRESHOLD=".90"
    export NIFI__nifiproperties__NIFI_MONITOR_LONG_RUNNING_TASK_SCHEDULE=""
    export NIFI__nifiproperties__NIFI_MONITOR_LONG_RUNNING_TASK_THRESHOLD=""
    export NIFI__nifiproperties__NIFI_DIAGNOSTICS_ON_SHUTDOWN_ENABLED="false"
    export NIFI__nifiproperties__NIFI_DIAGNOSTICS_ON_SHUTDOWN_VERBOSE="false"
    export NIFI__nifiproperties__NIFI_DIAGNOSTICS_ON_SHUTDOWN_DIRECTORY="_/diagnosticsnifi_diagnostics_on_shutdown_max_filecount=10nifi_diagnostics_on_shutdown_max_directory_size=10 MB"
	`

	nifiServers := ``

	literalWebHttpHost := `export NIFI__nifiproperties__NIFI_WEB_HTTP_HOST` +
		`=` + `"` +
		hostname +
		`.nifi-hs.` +
		namespace +
		`.svc.cluster.local` + `"`

	literalClusterAddress := `export NIFI__nifiproperties__NIFI_CLUSTER_ADDRESS` +
		`=` + `"` +
		hostname +
		`.nifi-hs.` +
		namespace +
		`.svc.cluster.local` + `"`

	nifi := &bigdatav1alpha1.Nifi{}
	zookeeper := nifi.Spec.Zookeeper

	zookeeperConnect := ``
	for i := 0; i < 2; i++ {
		literal := `export NIFI__nifiproperties__NIFI_ZOOKEEPER_CONNECT_STRING="` +
			//strings.Split(zookeeper, "/")[1] + `-` +
			`zk-` +
			strconv.Itoa(i) + `.zk-hs.` +
			strings.Split(zookeeper, "/")[0] +
			`.svc.cluster.local:2181` + `"`

		zookeeperConnect = zookeeperConnect + "\n" + literal
	}

	nifiServers = nifiServers + "\n" + literalWebHttpHost + "\n" + literalClusterAddress + "\n" + zookeeperConnect

	nifiEnv := nifiEnvPartial + nifiServers

	configMapData["nifi.env"] = nifiEnv
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nifi-config",
			Namespace: v.Namespace,
		},
		Data: configMapData,
	}

	if err := ctrl.SetControllerReference(v, configMap, r.Scheme); err != nil {
		return nil, err
	}

	return configMap, nil
}

func (r *NifiReconciler) serviceHeadlessForNifi(
	v *bigdatav1alpha1.Nifi) (*corev1.Service, error) {

	labels := labels(v, "nifi")
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nifi-hs",
			Namespace: v.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name: "nifi-http",
					Port: 8081,
				},
				{
					Name: "nifi-site",
					Port: 2881,
				},
				{
					Name: "nifi-node",
					Port: 2882,
				},
			},
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "None",
		},
	}

	if err := ctrl.SetControllerReference(v, s, r.Scheme); err != nil {
		return nil, err
	}

	return s, nil
}

func (r *NifiReconciler) serviceForNifi(
	v *bigdatav1alpha1.Nifi) (*corev1.Service, error) {

	labels := labels(v, "nifi")
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nifi-svc",
			Namespace: v.Namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name:     "nifi-port",
				Port:     8080,
				NodePort: 30000,
			}},
			Type: corev1.ServiceTypeNodePort,
		},
	}

	if err := ctrl.SetControllerReference(v, s, r.Scheme); err != nil {
		return nil, err
	}

	return s, nil
}

// stateFulSetForNifi returns a Nifi StateFulSet object
func (r *NifiReconciler) stateFulSetForNifi(
	nifi *bigdatav1alpha1.Nifi) (*appsv1.StatefulSet, error) {

	ls := labelsForNifi(nifi.Name)
	labels := labels(nifi, "nifi")

	replicas := nifi.Spec.Size

	// Get the Operand image
	image, err := imageForNifi()
	if err != nil {
		return nil, err
	}

	fastdisks := "fast-disks"

	dep := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nifi.Name,
			Namespace: nifi.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "nifi-hs",
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: ls,
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: nil,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ls,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Image:           image,
						Name:            "nifi",
						ImagePullPolicy: corev1.PullAlways,
						Ports: []corev1.ContainerPort{
							{
								ContainerPort: 8080,
								Name:          "nifi-port",
							},
							{
								ContainerPort: 8081,
								Name:          "nifi-http",
							},
							{
								ContainerPort: 2881,
								Name:          "nifi-site",
							},
							{
								ContainerPort: 2882,
								Name:          "nifi-node",
							},
							{
								ContainerPort: 9092,
								Name:          "exporter-port",
							},
						},
						Env: []corev1.EnvVar{
							{
								Name:  "SINGLE_USER_CREDENTIALS_USERNAME",
								Value: "admin",
							},
							{
								Name:  "SINGLE_USER_CREDENTIALS_PASSWORD",
								Value: "admin123456789",
							},
							{
								Name:  "NIFI_SENSITIVE_PROPS_KEY",
								Value: "randomstring12charsmin",
							},
							{
								Name: "HOSTNAME",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.name",
									},
								},
							},
							{
								Name:  "NIFI_WEB_HTTP_HOST",
								Value: "$(HOSTNAME).nifi-hs.default.svc.cluster.local",
							},
							{
								Name:  "NIFI_WEB_HTTP_PORT",
								Value: "8080",
							},
							{
								Name:  "NIFI_CLUSTER_IS_NODE",
								Value: "true",
							},
							{
								Name:  "NIFI_CLUSTER_NODE_PROTOCOL_PORT",
								Value: "8082",
							},
							{
								Name:  "NIFI_CLUSTER_NODE_PROTOCOL_MAX_THREADS",
								Value: "20",
							},
							{
								Name:  "NIFI_CLUSTER_ADDRESS",
								Value: "$(HOSTNAME).nifi-hs.default.svc.cluster.local",
							},
							{
								Name:  "NIFI_ANALYTICS_PREDICT_ENABLED",
								Value: "true",
							},
							{
								Name:  "NIFI_ELECTION_MAX_CANDIDATES",
								Value: "1",
							},
							{
								Name:  "NIFI_ZK_CONNECT_STRING",
								Value: "zk-0.zk-hs.default.svc.cluster.local:2181,zk-1.zk-hs.default.svc.cluster.local:2181,zk-2.zk-hs.default.svc.cluster.local:2181",
							},
							{
								Name:  "ZK_MONITOR_PORT",
								Value: "2888",
							},
							{
								Name:  "IS_CLUSTER_NODE",
								Value: "yes",
							},
							{
								Name:  "NIFI_ELECTION_MAX_WAIT",
								Value: "20 sec",
							},
							{
								Name:  "NIFI_JVM_HEAP_INIT",
								Value: "3g",
							},
							{
								Name:  "NIFI_JVM_HEAP_MAX",
								Value: "4g",
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{
								Name:      "data",
								MountPath: "/opt/nifi/nifi-current/data",
							},
							{
								Name:      "nifi-config-volume",
								MountPath: "/etc/environments",
							},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "nifi-config-volume",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: "nifi-config",
									},
								},
							},
						},
					},
				},
			},
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{
					Name:   "data",
					Labels: labels,
				},
				Spec: corev1.PersistentVolumeClaimSpec{
					AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceStorage: resource.MustParse("150Mi"),
						},
					},
					StorageClassName: &fastdisks,
				},
			}},
		},
	}

	// Set the ownerRef for the Deployment
	// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/
	if err := ctrl.SetControllerReference(nifi, dep, r.Scheme); err != nil {
		return nil, err
	}
	return dep, nil
}

// labelsForNifi returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForNifi(name string) map[string]string {
	var imageTag string
	image, err := imageForNifi()
	if err == nil {
		imageTag = strings.Split(image, ":")[1]
	}
	return map[string]string{"app.kubernetes.io/name": "Nifi",
		"app.kubernetes.io/instance":   name,
		"app.kubernetes.io/version":    imageTag,
		"app.kubernetes.io/part-of":    "nifi-operator",
		"app.kubernetes.io/created-by": "controller-manager",
		"app":                          "nifi",
	}
}

// imageForNifi gets the Operand image which is managed by this controller
// from the NIFI_IMAGE environment variable defined in the config/manager/manager.yaml
func imageForNifi() (string, error) {
	/*var imageEnvVar = "NIFI_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("Unable to find %s environment variable with the image", imageEnvVar)
	}
	*/
	image := "kubernetesbigdataeg/nifi-alpine:1.16.1-1"

	return image, nil
}

func labels(v *bigdatav1alpha1.Nifi, l string) map[string]string {
	return map[string]string{
		"app": l,
	}
}

// SetupWithManager sets up the controller with the Manager.
// Note that the Deployment will be also watched in order to ensure its
// desirable state on the cluster
func (r *NifiReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bigdatav1alpha1.Nifi{}).
		Owns(&appsv1.Deployment{}).
		Complete(r)
}
