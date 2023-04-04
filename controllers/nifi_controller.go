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
	"os"
	"strconv"
	"strings"

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

	// Crea o actualiza ConfigMap
	configMapFound := &corev1.ConfigMap{}
	if err := r.ensureResource(ctx, nifi, r.defaultConfigMapForNifi, configMapFound, "nifi-config", "ConfigMap"); err != nil {
		return ctrl.Result{}, err
	}

	// Headless Service
	serviceHeadlessFound := &corev1.Service{}
	if err := r.ensureResource(ctx, nifi, r.serviceHeadlessForNifi, serviceHeadlessFound, "nifi-hs", "Headless Service"); err != nil {
		return ctrl.Result{}, err
	}

	// Service
	serviceFound := &corev1.Service{}
	if err := r.ensureResource(ctx, nifi, r.serviceForNifi, serviceFound, "nifi-svc", "Service"); err != nil {
		return ctrl.Result{}, err
	}

	// StatefulSet
	found := &appsv1.StatefulSet{}
	if err := r.ensureResource(ctx, nifi, r.stateFulSetForNifi, found, nifi.Name, "StatefulSet"); err != nil {
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
	if found.Spec.Replicas == nil {
		log.Error(nil, "Spec is not initialized for StatefulSet", "StatefulSet.Namespace", found.Namespace, "StatefulSet.Name", found.Name)
		return ctrl.Result{}, fmt.Errorf("spec is not initialized for StatefulSet %s/%s", found.Namespace, found.Name)
	}
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

func (r *NifiReconciler) defaultConfigMapForNifi(Nifi *bigdatav1alpha1.Nifi) (client.Object, error) {
	namespace := Nifi.Namespace
	hostname := Nifi.Name

	configMapData := make(map[string]string, 0)
	nifiEnvPartial := `
	export NIFI__nifiproperties__nifi_sensitive_props_key="randomstring12charsmin"
    export NIFI__nifiproperties__nifi_web_http_host="${HOSTNAME}.nifi-hs.default.svc.cluster.local"
    export NIFI__nifiproperties__nifi_web_http_port="8080"
    export NIFI__nifiproperties__nifi_cluster_is_node="true"
    export NIFI__nifiproperties__nifi_analytics_predict_enabled="true"
    export NIFI__nifiproperties__nifi_zookeeper_connect_string="zk-0.zk-hs.default.svc.cluster.local:2181,zk-1.zk-hs.default.svc.cluster.local:2181,zk-2.zk-hs.default.svc.cluster.local:2181"
    export NIFI__nifiproperties__nifi_flow_configuration_file="./conf/flow.xml.gz"
    export NIFI__nifiproperties__nifi_flow_configuration_json_file="./conf/flow.json.gz"
    export NIFI__nifiproperties__nifi_flow_configuration_archive_enabled="true"
    export NIFI__nifiproperties__nifi_flow_configuration_archive_dir="./conf/archive/"
    export NIFI__nifiproperties__nifi_flow_configuration_archive_max_time="30 days"
    export NIFI__nifiproperties__nifi_flow_configuration_archive_max_storage="500 mb"
    export NIFI__nifiproperties__nifi_flow_configuration_archive_max_count=""
    export NIFI__nifiproperties__nifi_flowcontroller_autoresumestate="true"
    export NIFI__nifiproperties__nifi_flowcontroller_graceful_shutdown_period="10 sec"
    export NIFI__nifiproperties__nifi_flowservice_writedelay_interval="500 ms"
    export NIFI__nifiproperties__nifi_administrative_yield_duration="30 sec"
    export NIFI__nifiproperties__nifi_bored_yield_duration="10 millis"
    export NIFI__nifiproperties__nifi_queue_backpressure_count="10000"
    export NIFI__nifiproperties__nifi_queue_backpressure_size="1 GB"
    export NIFI__nifiproperties__nifi_authorizer_configuration_file="./conf/authorizers.xml"
    export NIFI__nifiproperties__nifi_login_identity_provider_configuration_file="./conf/login-identity-providers.xml"
    export NIFI__nifiproperties__nifi_templates_directory="./conf/templates"
    export NIFI__nifiproperties__nifi_ui_banner_text=""
    export NIFI__nifiproperties__nifi_ui_autorefresh_interval="30 sec"
    export NIFI__nifiproperties__nifi_nar_library_directory="./lib"
    export NIFI__nifiproperties__nifi_nar_library_autoload_directory="./extensions"
    export NIFI__nifiproperties__nifi_nar_working_directory="./work/nar/"
    export NIFI__nifiproperties__nifi_documentation_working_directory="./work/docs/components"
    export NIFI__nifiproperties__nifi_state_management_configuration_file="./conf/state-management.xml"
    export NIFI__nifiproperties__nifi_state_management_provider_local="local-provider"
    export NIFI__nifiproperties__nifi_state_management_provider_cluster="zk-provider"
    export NIFI__nifiproperties__nifi_state_management_embedded_zookeeper_start="false"
    export NIFI__nifiproperties__nifi_state_management_embedded_zookeeper_properties="./conf/zookeeper.properties"
    export NIFI__nifiproperties__nifi_database_directory="./database_repository"
    export NIFI__nifiproperties__nifi_h2_url_append=";LOCK_TIMEOUT=25000;WRITE_DELAY=0;AUTO_SERVER=FALSE"
    export NIFI__nifiproperties__nifi_repository_encryption_protocol_version=""
    export NIFI__nifiproperties__nifi_repository_encryption_key_id=""
    export NIFI__nifiproperties__nifi_repository_encryption_key_provider=""
    export NIFI__nifiproperties__nifi_repository_encryption_key_provider_keystore_location=""
    export NIFI__nifiproperties__nifi_repository_encryption_key_provider_keystore_password=""
    export NIFI__nifiproperties__nifi_flowfile_repository_implementation="org.apache.nifi.controller.repository.WriteAheadFlowFileRepository"
    export NIFI__nifiproperties__nifi_flowfile_repository_wal_implementation="org.apache.nifi.wali.SequentialAccessWriteAheadLog"
    export NIFI__nifiproperties__nifi_flowfile_repository_directory="./flowfile_repository"
    export NIFI__nifiproperties__nifi_flowfile_repository_checkpoint_interval="20 secs"
    export NIFI__nifiproperties__nifi_flowfile_repository_always_sync="false"
    export NIFI__nifiproperties__nifi_flowfile_repository_retain_orphaned_flowfiles="true"
    export NIFI__nifiproperties__nifi_swap_manager_implementation="org.apache.nifi.controller.FileSystemSwapManager"
    export NIFI__nifiproperties__nifi_queue_swap_threshold="20000"
    export NIFI__nifiproperties__nifi_content_repository_implementation="org.apache.nifi.controller.repository.FileSystemRepository"
    export NIFI__nifiproperties__nifi_content_claim_max_appendable_size="50 KB"
    export NIFI__nifiproperties__nifi_content_repository_directory_default="./content_repository"
    export NIFI__nifiproperties__nifi_content_repository_archive_max_retention_period="7 days"
    export NIFI__nifiproperties__nifi_content_repository_archive_max_usage_percentage="50%"
    export NIFI__nifiproperties__nifi_content_repository_archive_enabled="true"
    export NIFI__nifiproperties__nifi_content_repository_always_sync="false"
    export NIFI__nifiproperties__nifi_content_viewer_url="../nifi-content-viewer/"
    export NIFI__nifiproperties__nifi_provenance_repository_implementation="org.apache.nifi.provenance.WriteAheadProvenanceRepository"
    export NIFI__nifiproperties__nifi_provenance_repository_directory_default="./provenance_repository"
    export NIFI__nifiproperties__nifi_provenance_repository_max_storage_time="30 days"
    export NIFI__nifiproperties__nifi_provenance_repository_max_storage_size="10 GB"
    export NIFI__nifiproperties__nifi_provenance_repository_rollover_time="10 mins"
    export NIFI__nifiproperties__nifi_provenance_repository_rollover_size="100 MB"
    export NIFI__nifiproperties__nifi_provenance_repository_query_threads="2"
    export NIFI__nifiproperties__nifi_provenance_repository_index_threads="2"
    export NIFI__nifiproperties__nifi_provenance_repository_compress_on_rollover="true"
    export NIFI__nifiproperties__nifi_provenance_repository_always_sync="false"
    export NIFI__nifiproperties__nifi_provenance_repository_indexed_fields="EventType, FlowFileUUID, Filename, ProcessorID, Relationship"
    export NIFI__nifiproperties__nifi_provenance_repository_indexed_attributes=""
    export NIFI__nifiproperties__nifi_provenance_repository_index_shard_size="500 MB"
    export NIFI__nifiproperties__nifi_provenance_repository_max_attribute_length="65536"
    export NIFI__nifiproperties__nifi_provenance_repository_concurrent_merge_threads="2"
    export NIFI__nifiproperties__nifi_provenance_repository_buffer_size="100000"
    export NIFI__nifiproperties__nifi_components_status_repository_implementation="org.apache.nifi.controller.status.history.VolatileComponentStatusRepository"
    export NIFI__nifiproperties__nifi_components_status_repository_buffer_size="1440"
    export NIFI__nifiproperties__nifi_components_status_snapshot_frequency="1 min"
    export NIFI__nifiproperties__nifi_status_repository_questdb_persist_node_days="14"
    export NIFI__nifiproperties__nifi_status_repository_questdb_persist_component_days="3"
    export NIFI__nifiproperties__nifi_status_repository_questdb_persist_location="./status_repository"
    export NIFI__nifiproperties__nifi_remote_input_host="${HOSTNAME}"
    export NIFI__nifiproperties__nifi_remote_input_secure="false"
    export NIFI__nifiproperties__nifi_remote_input_socket_port="10000"
    export NIFI__nifiproperties__nifi_remote_input_http_enabled="true"
    export NIFI__nifiproperties__nifi_remote_input_http_transaction_ttl="30 sec"
    export NIFI__nifiproperties__nifi_remote_contents_cache_expiration="30 secs"
    export NIFI__nifiproperties__nifi_web_http_network_interface_default=""
    export NIFI__nifiproperties__nifi_web_https_host=""
    export NIFI__nifiproperties__nifi_web_https_port=""
    export NIFI__nifiproperties__nifi_web_https_network_interface_default=""
    export NIFI__nifiproperties__nifi_web_jetty_working_directory="./work/jetty"
    export NIFI__nifiproperties__nifi_web_jetty_threads="200"
    export NIFI__nifiproperties__nifi_web_max_header_size="16 KB"
    export NIFI__nifiproperties__nifi_web_proxy_context_path=""
    export NIFI__nifiproperties__nifi_web_proxy_host=""
    export NIFI__nifiproperties__nifi_web_max_content_size=""
    export NIFI__nifiproperties__nifi_web_max_requests_per_second="30000"
    export NIFI__nifiproperties__nifi_web_max_access_token_requests_per_second="25"
    export NIFI__nifiproperties__nifi_web_request_timeout="60 secs"
    export NIFI__nifiproperties__nifi_web_request_ip_whitelist=""
    export NIFI__nifiproperties__nifi_web_should_send_server_version="true"
    export NIFI__nifiproperties__nifi_web_request_log_format="%{client}a - %u %t \"%r\" %s %O \"%{Referer}i\" \"%{User-Agent}i\""
    export NIFI__nifiproperties__nifi_web_https_ciphersuites_include=""
    export NIFI__nifiproperties__nifi_web_https_ciphersuites_exclude=""
    export NIFI__nifiproperties__nifi_sensitive_props_key_protected=""
    export NIFI__nifiproperties__nifi_sensitive_props_algorithm="NIFI_PBKDF2_AES_GCM_256"
    export NIFI__nifiproperties__nifi_sensitive_props_additional_keys=""
    export NIFI__nifiproperties__nifi_security_autoreload_enabled="false"
    export NIFI__nifiproperties__nifi_security_autoreload_interval="10 secs"
    export NIFI__nifiproperties__nifi_security_keystore=""
    export NIFI__nifiproperties__nifi_security_keystoretype=""
    export NIFI__nifiproperties__nifi_security_keystorepasswd=""
    export NIFI__nifiproperties__nifi_security_keypasswd=""
    export NIFI__nifiproperties__nifi_security_truststore=""
    export NIFI__nifiproperties__nifi_security_truststoretype=""
    export NIFI__nifiproperties__nifi_security_truststorepasswd=""
    export NIFI__nifiproperties__nifi_security_user_authorizer="single-user-authorizer"
    export NIFI__nifiproperties__nifi_security_allow_anonymous_authentication="false"
    export NIFI__nifiproperties__nifi_security_user_login_identity_provider=""
    export NIFI__nifiproperties__nifi_security_user_jws_key_rotation_period="PT1H"
    export NIFI__nifiproperties__nifi_security_ocsp_responder_url=""
    export NIFI__nifiproperties__nifi_security_ocsp_responder_certificate=""
    export NIFI__nifiproperties__nifi_security_user_oidc_discovery_url=""
    export NIFI__nifiproperties__nifi_security_user_oidc_connect_timeout="5 secs"
    export NIFI__nifiproperties__nifi_security_user_oidc_read_timeout="5 secs"
    export NIFI__nifiproperties__nifi_security_user_oidc_client_id=""
    export NIFI__nifiproperties__nifi_security_user_oidc_client_secret=""
    export NIFI__nifiproperties__nifi_security_user_oidc_preferred_jwsalgorithm=""
    export NIFI__nifiproperties__nifi_security_user_oidc_additional_scopes=""
    export NIFI__nifiproperties__nifi_security_user_oidc_claim_identifying_user=""
    export NIFI__nifiproperties__nifi_security_user_oidc_fallback_claims_identifying_user=""
    export NIFI__nifiproperties__nifi_security_user_oidc_truststore_strategy="JDK"
    export NIFI__nifiproperties__nifi_security_user_knox_url=""
    export NIFI__nifiproperties__nifi_security_user_knox_publickey=""
    export NIFI__nifiproperties__nifi_security_user_knox_cookiename="hadoop-jwt"
    export NIFI__nifiproperties__nifi_security_user_knox_audiences=""
    export NIFI__nifiproperties__nifi_security_user_saml_idp_metadata_url=""
    export NIFI__nifiproperties__nifi_security_user_saml_sp_entity_id=""
    export NIFI__nifiproperties__nifi_security_user_saml_identity_attribute_name=""
    export NIFI__nifiproperties__nifi_security_user_saml_group_attribute_name=""
    export NIFI__nifiproperties__nifi_security_user_saml_metadata_signing_enabled="false"
    export NIFI__nifiproperties__nifi_security_user_saml_request_signing_enabled="false"
    export NIFI__nifiproperties__nifi_security_user_saml_signature_algorithm="http://www.w3.org/2001/04/xmldsig-more#rsa-sha256"
    export NIFI__nifiproperties__nifi_security_user_saml_signature_digest_algorithm="http://www.w3.org/2001/04/xmlenc#sha256"
    export NIFI__nifiproperties__nifi_security_user_saml_message_logging_enabled="false"
    export NIFI__nifiproperties__nifi_security_user_saml_authentication_expiration="12 hours"
    export NIFI__nifiproperties__nifi_security_user_saml_single_logout_enabled="false"
    export NIFI__nifiproperties__nifi_security_user_saml_http_client_truststore_strategy="JDK"
    export NIFI__nifiproperties__nifi_security_user_saml_http_client_connect_timeout="30 secs"
    export NIFI__nifiproperties__nifi_security_user_saml_http_client_read_timeout="30 secs"
    export NIFI__nifiproperties__nifi_listener_bootstrap_port="0"
    export NIFI__nifiproperties__nifi_cluster_protocol_heartbeat_interval="5 sec"
    export NIFI__nifiproperties__nifi_cluster_protocol_heartbeat_missable_max="8"
    export NIFI__nifiproperties__nifi_cluster_protocol_is_secure="false"
    export NIFI__nifiproperties__nifi_cluster_node_address="${HOSTNAME}.nifi-hs.default.svc.cluster.local"
    export NIFI__nifiproperties__nifi_cluster_node_event_history_size="25"
    export NIFI__nifiproperties__nifi_cluster_node_connection_timeout="5 sec"
    export NIFI__nifiproperties__nifi_cluster_node_read_timeout="5 sec"
    export NIFI__nifiproperties__nifi_cluster_node_max_concurrent_requests="100"
    export NIFI__nifiproperties__nifi_cluster_firewall_file=""
    export NIFI__nifiproperties__nifi_cluster_flow_election_max_wait_time="20 sec"
    export NIFI__nifiproperties__nifi_cluster_flow_election_max_candidates="1"
    export NIFI__nifiproperties__nifi_cluster_load_balance_host=""
    export NIFI__nifiproperties__nifi_cluster_load_balance_port="6342"
    export NIFI__nifiproperties__nifi_cluster_load_balance_connections_per_node="1"
    export NIFI__nifiproperties__nifi_cluster_load_balance_max_thread_count="8"
    export NIFI__nifiproperties__nifi_cluster_load_balance_comms_timeout="30 sec"
    export NIFI__nifiproperties__nifi_zookeeper_connect_timeout="10 secs"
    export NIFI__nifiproperties__nifi_zookeeper_session_timeout="10 secs"
    export NIFI__nifiproperties__nifi_zookeeper_root_node="/nifi"
    export NIFI__nifiproperties__nifi_zookeeper_client_secure="false"
    export NIFI__nifiproperties__nifi_zookeeper_security_keystore=""
    export NIFI__nifiproperties__nifi_zookeeper_security_keystoretype=""
    export NIFI__nifiproperties__nifi_zookeeper_security_keystorepasswd=""
    export NIFI__nifiproperties__nifi_zookeeper_security_truststore=""
    export NIFI__nifiproperties__nifi_zookeeper_security_truststoretype=""
    export NIFI__nifiproperties__nifi_zookeeper_security_truststorepasswd=""
    export NIFI__nifiproperties__nifi_zookeeper_jute_maxbuffer=""
    export NIFI__nifiproperties__nifi_zookeeper_auth_type=""
    export NIFI__nifiproperties__nifi_zookeeper_kerberos_removehostfromprincipal=""
    export NIFI__nifiproperties__nifi_zookeeper_kerberos_removerealmfromprincipal=""
    export NIFI__nifiproperties__nifi_kerberos_krb5_file=""
    export NIFI__nifiproperties__nifi_kerberos_service_principal=""
    export NIFI__nifiproperties__nifi_kerberos_service_keytab_location=""
    export NIFI__nifiproperties__nifi_kerberos_spnego_principal=""
    export NIFI__nifiproperties__nifi_kerberos_spnego_keytab_location=""
    export NIFI__nifiproperties__nifi_kerberos_spnego_authentication_expiration="12 hours"
    export NIFI__nifiproperties__nifi_variable_registry_properties=""
    export NIFI__nifiproperties__nifi_analytics_predict_interval="3 mins"
    export NIFI__nifiproperties__nifi_analytics_query_interval="5 mins"
    export NIFI__nifiproperties__nifi_analytics_connection_model_implementation="org.apache.nifi.controller.status.analytics.models.OrdinaryLeastSquares"
    export NIFI__nifiproperties__nifi_analytics_connection_model_score_name="rSquared"
    export NIFI__nifiproperties__nifi_analytics_connection_model_score_threshold=".90"
    export NIFI__nifiproperties__nifi_monitor_long_running_task_schedule=""
    export NIFI__nifiproperties__nifi_monitor_long_running_task_threshold=""
    export NIFI__nifiproperties__nifi_diagnostics_on_shutdown_enabled="false"
    export NIFI__nifiproperties__nifi_diagnostics_on_shutdown_verbose="false"
    export NIFI__nifiproperties__nifi_diagnostics_on_shutdown_directory="./diagnostics"
    export NIFI__nifiproperties__nifi_diagnostics_on_shutdown_max_filecount="10"
    export NIFI__nifiproperties__nifi_diagnostics_on_shutdown_max_directory_size="10 MB"
    export NIFI__nifiproperties__nifi_security_user_saml_want_assertions_signed="true"
    export NIFI__nifiproperties__nifi_cluster_node_protocol_port=8082
    export NIFI__nifiproperties__nifi_cluster_node_protocol_max_threads=20
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
			strings.Split(zookeeper, "/")[1] + `-` +
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
			Namespace: Nifi.Namespace,
		},
		Data: configMapData,
	}

	if err := ctrl.SetControllerReference(Nifi, configMap, r.Scheme); err != nil {
		return nil, err
	}

	return configMap, nil
}

func (r *NifiReconciler) serviceHeadlessForNifi(Nifi *bigdatav1alpha1.Nifi) (client.Object, error) {

	labels := labelsForNifi(Nifi.Name)
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nifi-hs",
			Namespace: Nifi.Namespace,
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

	if err := ctrl.SetControllerReference(Nifi, s, r.Scheme); err != nil {
		return nil, err
	}

	return s, nil
}

func (r *NifiReconciler) serviceForNifi(Nifi *bigdatav1alpha1.Nifi) (client.Object, error) {

	labels := labelsForNifi(Nifi.Name)
	s := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nifi-svc",
			Namespace: Nifi.Namespace,
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

	if err := ctrl.SetControllerReference(Nifi, s, r.Scheme); err != nil {
		return nil, err
	}

	return s, nil
}

// stateFulSetForNifi returns a Nifi StateFulSet object
// func (r *NifiReconciler) stateFulSetForNifi(nifi *bigdatav1alpha1.Nifi) (*appsv1.StatefulSet, error) {
func (r *NifiReconciler) stateFulSetForNifi(nifi *bigdatav1alpha1.Nifi) (client.Object, error) {

	labels := labelsForNifi(nifi.Name)

	replicas := nifi.Spec.Size

	// Get the Operand image
	image, err := imageForNifi()
	if err != nil {
		return nil, err
	}

	fastdisks := "fast-disks"

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      nifi.Name,
			Namespace: nifi.Namespace,
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "nifi-hs",
			Replicas:    &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type:          "RollingUpdate",
				RollingUpdate: nil,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
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
								Name: "HOSTNAME",
								ValueFrom: &corev1.EnvVarSource{
									FieldRef: &corev1.ObjectFieldSelector{
										FieldPath: "metadata.name",
									},
								},
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
	if err := ctrl.SetControllerReference(nifi, sts, r.Scheme); err != nil {
		return nil, err
	}
	return sts, nil
}

// labelsForNifi returns the labels for selecting the resources
// More info: https://kubernetes.io/docs/concepts/overview/working-with-objects/common-labels/
func labelsForNifi(name string) map[string]string {
	var imageTag string
	image, err := imageForNifi()
	if err == nil {
		imageTag = strings.Split(image, ":")[1]
	}
	return map[string]string{
		"app.kubernetes.io/name":       "Nifi",
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
	var imageEnvVar = "NIFI_IMAGE"
	image, found := os.LookupEnv(imageEnvVar)
	if !found {
		return "", fmt.Errorf("unable to find %s environment variable with the image", imageEnvVar)
	}

	//image := "kubernetesbigdataeg/nifi-alpine:1.16.1-1"

	return image, nil
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

func (r *NifiReconciler) ensureResource(ctx context.Context, nifi *bigdatav1alpha1.Nifi, createResourceFunc func(*bigdatav1alpha1.Nifi) (client.Object, error), foundResource client.Object, resourceName string, resourceType string) error {
	log := log.FromContext(ctx)
	err := r.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: nifi.Namespace}, foundResource)
	if err != nil && apierrors.IsNotFound(err) {
		resource, err := createResourceFunc(nifi)
		if err != nil {
			log.Error(err, fmt.Sprintf("Failed to define new %s resource for Nifi", resourceType))

			// The following implementation will update the status
			meta.SetStatusCondition(&nifi.Status.Conditions, metav1.Condition{Type: typeAvailableNifi,
				Status: metav1.ConditionFalse, Reason: "Reconciling",
				Message: fmt.Sprintf("Failed to create %s for the custom resource (%s): (%s)", resourceType, nifi.Name, err)})

			if err := r.Status().Update(ctx, nifi); err != nil {
				log.Error(err, "Failed to update Nifi status")
				return err
			}

			return err
		}

		log.Info(fmt.Sprintf("Creating a new %s", resourceType),
			fmt.Sprintf("%s.Namespace", resourceType), resource.GetNamespace(), fmt.Sprintf("%s.Name", resourceType), resource.GetName())

		if err = r.Create(ctx, resource); err != nil {
			log.Error(err, fmt.Sprintf("Failed to create new %s", resourceType),
				fmt.Sprintf("%s.Namespace", resourceType), resource.GetNamespace(), fmt.Sprintf("%s.Name", resourceType), resource.GetName())
			return err
		}

		if err := r.Get(ctx, types.NamespacedName{Name: resourceName, Namespace: nifi.Namespace}, foundResource); err != nil {
			log.Error(err, fmt.Sprintf("Failed to get newly created %s", resourceType))
			return err
		}

	} else if err != nil {
		log.Error(err, fmt.Sprintf("Failed to get %s", resourceType))
		return err
	}

	return nil
}
