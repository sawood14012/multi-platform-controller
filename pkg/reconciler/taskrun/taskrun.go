package taskrun

import (
	"context"
	"fmt"
	errors2 "github.com/pkg/errors"
	"github.com/redhat-appstudio/multi-platform-controller/pkg/aws"
	"github.com/redhat-appstudio/multi-platform-controller/pkg/cloud"
	"github.com/redhat-appstudio/multi-platform-controller/pkg/ibm"
	"regexp"

	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1"
	v12 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"knative.dev/pkg/apis"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
)

const (
	//TODO eventually we'll need to decide if we want to make this tuneable
	contextTimeout = 300 * time.Second

	SecretPrefix   = "multi-platform-ssh-"
	ConfigMapLabel = "build.appstudio.redhat.com/multi-platform-config"

	MultiPlatformSecretLabel = "build.appstudio.redhat.com/multi-platform-secret"

	AssignedHost           = "build.appstudio.redhat.com/assigned-host"
	FailedHosts            = "build.appstudio.redhat.com/failed-hosts"
	CloudInstanceId        = "build.appstudio.redhat.com/cloud-instance-id"
	CloudFailures          = "build.appstudio.redhat.com/cloud-failure-count"
	CloudAddress           = "build.appstudio.redhat.com/cloud-address"
	CloudDynamicPlatform   = "build.appstudio.redhat.com/cloud-dynamic-platform"
	ProvisionTaskProcessed = "build.appstudio.redhat.com/provision-task-processed"
	ProvisionTaskFinalizer = "build.appstudio.redhat.com/provision-task-finalizer"

	UserTaskName      = "build.appstudio.redhat.com/user-task-name"
	UserTaskNamespace = "build.appstudio.redhat.com/user-task-namespace"

	WaitingForPlatformLabel = "build.appstudio.redhat.com/waiting-for-platform"
	PipelineFinalizer       = "appstudio.io/multi-platform-finalizer"
	HostConfig              = "host-config"

	TaskTypeLabel     = "build.appstudio.redhat.com/task-type"
	TaskTypeProvision = "provision"
	TaskTypeClean     = "clean"

	ServiceAccountName = "multi-platform-controller"

	PlatformParam     = "PLATFORM"
	DynamicPlatforms  = "dynamic-platforms"
	AllowedNamespaces = "allowed-namespaces"
)

type ReconcileTaskRun struct {
	apiReader         client.Reader
	client            client.Client
	scheme            *runtime.Scheme
	eventRecorder     record.EventRecorder
	operatorNamespace string

	cloudProviders map[string]func(platform string, config map[string]string, systemNamespace string) cloud.CloudProvider
}

func newReconciler(mgr ctrl.Manager, operatorNamespace string) reconcile.Reconciler {
	return &ReconcileTaskRun{
		apiReader:         mgr.GetAPIReader(),
		client:            mgr.GetClient(),
		scheme:            mgr.GetScheme(),
		eventRecorder:     mgr.GetEventRecorderFor("ComponentBuild"),
		operatorNamespace: operatorNamespace,
		cloudProviders:    map[string]func(platform string, config map[string]string, systemNamespace string) cloud.CloudProvider{"aws": aws.Ec2Provider, "ibmz": ibm.IBMZProvider, "ibmp": ibm.IBMPowerProvider},
	}
}

func (r *ReconcileTaskRun) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	// Set the ctx to be Background, as the top-level context for incoming requests.
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, contextTimeout)
	defer cancel()
	log := ctrl.Log.WithName("taskrun").WithValues("request", request.NamespacedName)
	log.Info("Reconciling")

	pr := v1.TaskRun{}
	prerr := r.client.Get(ctx, request.NamespacedName, &pr)
	if prerr != nil {
		if !errors.IsNotFound(prerr) {
			log.Error(prerr, "Reconcile key %s as TaskRun unexpected error", request.NamespacedName.String())
			return ctrl.Result{}, prerr
		}
	}
	if prerr != nil {
		msg := "Reconcile key received not found errors for TaskRuns (probably deleted): " + request.NamespacedName.String()
		log.Info(msg)
		return ctrl.Result{}, nil
	}

	switch {
	case prerr == nil:
		return r.handleTaskRunReceived(ctx, &log, &pr)
	}

	return reconcile.Result{}, nil
}

func (r *ReconcileTaskRun) handleTaskRunReceived(ctx context.Context, log *logr.Logger, tr *v1.TaskRun) (reconcile.Result, error) {
	if tr.Labels != nil {
		taskType := tr.Labels[TaskTypeLabel]
		if taskType == TaskTypeClean {
			return r.handleCleanTask(ctx, log, tr)
		}
		if taskType == TaskTypeProvision {
			return r.handleProvisionTask(ctx, log, tr)
		}
	}
	if tr.Spec.Params == nil {
		return reconcile.Result{}, nil
	}

	//identify tasks by the PLATFORM param and multi-platform-ssh- secret
	if tr.Status.TaskSpec == nil || tr.Status.TaskSpec.Volumes == nil {
		return reconcile.Result{}, nil
	}
	found := false
	for _, i := range tr.Status.TaskSpec.Volumes {
		if i.Secret != nil {
			if strings.HasPrefix(i.Secret.SecretName, SecretPrefix) {
				found = true
			}
		}
	}
	if !found {
		//this is not something we need to be concerned with
		return reconcile.Result{}, nil
	}
	found = false
	for _, i := range tr.Spec.Params {
		if i.Name == PlatformParam {
			found = true
		}
	}
	if !found {
		//this is not something we need to be concerned with
		return reconcile.Result{}, nil
	}

	if tr.Status.TaskSpec == nil || tr.Status.TaskSpec.Volumes == nil {
		return reconcile.Result{}, nil
	}
	return r.handleUserTask(ctx, log, tr)
}

// called when a task has finished, we look for waiting tasks
// and then potentially requeue one of them
func (r *ReconcileTaskRun) handleWaitingTasks(ctx context.Context, log *logr.Logger, platform string) (reconcile.Result, error) {

	//try and requeue a waiting task if one exists
	taskList := v1.TaskRunList{}

	err := r.client.List(ctx, &taskList, client.MatchingLabels{WaitingForPlatformLabel: platformLabel(platform)})
	if err != nil {
		return reconcile.Result{}, err
	}
	var oldest *v1.TaskRun
	var oldestTs time.Time
	for i := range taskList.Items {
		tr := taskList.Items[i]
		if oldest == nil || oldestTs.After(tr.CreationTimestamp.Time) {
			oldestTs = tr.CreationTimestamp.Time
			oldest = &tr
		}
	}
	if oldest != nil {
		//remove the waiting label, which will trigger a requeue
		delete(oldest.Labels, WaitingForPlatformLabel)
		return reconcile.Result{}, r.client.Update(ctx, oldest)
	}
	return reconcile.Result{}, nil

}

func (r *ReconcileTaskRun) handleCleanTask(ctx context.Context, log *logr.Logger, tr *v1.TaskRun) (reconcile.Result, error) {
	if tr.Status.CompletionTime == nil {
		return reconcile.Result{}, nil
	}
	success := tr.Status.GetCondition(apis.ConditionSucceeded).IsTrue()
	if !success {
		log.Info("cleanup task failed", "task", tr.Name)
	}
	//leave the TR for an hour
	if tr.Status.CompletionTime.Add(time.Hour).Before(time.Now()) {
		return reconcile.Result{}, r.client.Delete(ctx, tr)
	}
	return reconcile.Result{RequeueAfter: time.Hour}, nil
}

func (r *ReconcileTaskRun) handleProvisionTask(ctx context.Context, log *logr.Logger, tr *v1.TaskRun) (reconcile.Result, error) {

	if tr.Status.CompletionTime == nil {
		return reconcile.Result{}, nil
	}
	if tr.Annotations == nil {
		tr.Annotations = map[string]string{}
	}
	if tr.Annotations[ProvisionTaskProcessed] == "true" {
		//leave the TR for an hour so we can view logs
		//TODO: tekton results integration
		if tr.Status.CompletionTime.Add(time.Hour).Before(time.Now()) {
			return reconcile.Result{}, r.client.Delete(ctx, tr)
		}
		return reconcile.Result{RequeueAfter: time.Hour}, nil
	}
	tr.Annotations[ProvisionTaskProcessed] = "true"
	success := tr.Status.GetCondition(apis.ConditionSucceeded).IsTrue()
	secretName := ""
	for _, i := range tr.Spec.Params {
		if i.Name == "SECRET_NAME" {
			secretName = i.Value.StringVal
			break
		}
	}
	if !success {
		assigned := tr.Labels[AssignedHost]
		if assigned != "" {
			userTr := v1.TaskRun{}
			err := r.client.Get(ctx, types.NamespacedName{Namespace: tr.Labels[UserTaskNamespace], Name: tr.Labels[UserTaskName]}, &userTr)
			if err == nil {
				if userTr.Annotations == nil {
					userTr.Annotations = map[string]string{}
				}
				//add to failed hosts and remove assigned
				//this will cause it to try again
				failed := strings.Split(userTr.Annotations[FailedHosts], ",")
				if failed[0] == "" {
					failed = []string{}
				}
				failed = append(failed, assigned)
				userTr.Annotations[FailedHosts] = strings.Join(failed, ",")
				delete(userTr.Labels, AssignedHost)
				err = r.client.Update(ctx, &userTr)
				if err != nil {
					return reconcile.Result{}, err
				}
				delete(tr.Labels, AssignedHost)
				err := r.client.Update(ctx, tr)
				if err != nil {
					return reconcile.Result{}, err
				}
			}
		}
	} else {
		log.Info("provision task succeeded")
		//verify we ended up with a secret
		secret := v12.Secret{}
		err := r.client.Get(ctx, types.NamespacedName{Namespace: tr.Labels[UserTaskNamespace], Name: secretName}, &secret)
		if err != nil {
			if errors.IsNotFound(err) {
				userTr := v1.TaskRun{}
				err = r.client.Get(ctx, types.NamespacedName{Namespace: tr.Labels[UserTaskNamespace], Name: tr.Labels[UserTaskName]}, &userTr)
				if err != nil {
					if !errors.IsNotFound(err) {
						//if the task run is not found then this is just old
						return reconcile.Result{}, err
					}
				} else {
					err = r.createErrorSecret(ctx, &userTr, secretName, "provision task failed to create a secret")
					if err != nil {
						return reconcile.Result{}, err
					}

				}
			} else {
				return reconcile.Result{}, err
			}
		}
	}
	return reconcile.Result{}, r.client.Update(ctx, tr)
}

// This creates an secret with the 'error' field set
// This will result in the pipeline run immediately failing with the message printed in the logs
func (r *ReconcileTaskRun) createErrorSecret(ctx context.Context, tr *v1.TaskRun, secretName string, msg string) error {
	if controllerutil.AddFinalizer(tr, PipelineFinalizer) {
		err := r.client.Update(ctx, tr)
		if err != nil {
			return err
		}
	}

	secret := v12.Secret{}
	secret.Labels = map[string]string{MultiPlatformSecretLabel: "true"}
	secret.Namespace = tr.Namespace
	secret.Name = secretName
	err := controllerutil.SetOwnerReference(tr, &secret, r.scheme)
	if err != nil {
		return err
	}

	secret.Data = map[string][]byte{
		"error": []byte(msg),
	}
	err = r.client.Create(ctx, &secret)
	if err != nil {
		if errors.IsAlreadyExists(err) {
			//already exists, ignore
			return nil
		}
		return err
	}
	return nil
}

func (r *ReconcileTaskRun) handleUserTask(ctx context.Context, log *logr.Logger, tr *v1.TaskRun) (reconcile.Result, error) {

	secretName := SecretPrefix + tr.Name
	if tr.Labels[AssignedHost] != "" {
		return r.handleHostAssigned(ctx, log, tr, secretName)
	} else {
		//if the PR is done we ignore it
		if tr.Status.CompletionTime != nil || tr.GetDeletionTimestamp() != nil {
			if controllerutil.ContainsFinalizer(tr, PipelineFinalizer) {
				return r.handleHostAssigned(ctx, log, tr, secretName)

			}
			return reconcile.Result{}, nil
		}

		return r.handleHostAllocation(ctx, log, tr, secretName)
	}
}

func extracPlatform(tr *v1.TaskRun) (string, error) {
	for _, p := range tr.Spec.Params {
		if p.Name == PlatformParam {
			return p.Value.StringVal, nil
		}
	}
	return "", errors2.New("failed to determine platform")
}

func (r *ReconcileTaskRun) handleHostAllocation(ctx context.Context, log *logr.Logger, tr *v1.TaskRun, secretName string) (reconcile.Result, error) {
	log.Info("attempting to allocate host")

	if tr.Labels == nil {
		tr.Labels = map[string]string{}
	}
	if r.apiReader != nil {
		err := r.apiReader.Get(ctx, types.NamespacedName{Namespace: tr.Namespace, Name: tr.Name}, tr)
		if err != nil {
			return reconcile.Result{}, err
		}
	}
	//check the secret does not already exist
	secret := v12.Secret{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: tr.Namespace, Name: secretName}, &secret)
	if err != nil {
		if !errors.IsNotFound(err) {
			return reconcile.Result{}, err
		}
	} else {
		//secret already exists (probably error secret)
		return reconcile.Result{}, nil
	}
	targetPlatform, err := extracPlatform(tr)
	if err != nil {
		return reconcile.Result{}, r.createErrorSecret(ctx, tr, secretName, "failed to determine platform, no PLATFORM param")
	}

	//lets allocate a host, get the map with host info
	hosts, instanceTag, err := r.readConfiguration(ctx, log, targetPlatform, tr.Namespace)
	if err != nil {
		log.Error(err, "failed to read host config")
		return reconcile.Result{}, r.createErrorSecret(ctx, tr, secretName, "failed to read host config "+err.Error())
	}
	return hosts.Allocate(r, ctx, log, tr, secretName, instanceTag)

}

func (r *ReconcileTaskRun) handleHostAssigned(ctx context.Context, log *logr.Logger, tr *v1.TaskRun, secretName string) (reconcile.Result, error) {
	//already exists
	if tr.Status.CompletionTime != nil || tr.GetDeletionTimestamp() != nil {
		log.Info("unassigning host from task")

		selectedHost := tr.Labels[AssignedHost]
		platform, err := extracPlatform(tr)
		if err != nil {
			return reconcile.Result{}, err
		}
		config, _, err := r.readConfiguration(ctx, log, platform, tr.Namespace)
		if err != nil {
			return reconcile.Result{}, err
		}
		err = config.Deallocate(r, ctx, log, tr, secretName, selectedHost)
		if err != nil {
			log.Error(err, "Failed to deallocate host "+selectedHost)
		}
		controllerutil.RemoveFinalizer(tr, PipelineFinalizer)
		delete(tr.Labels, AssignedHost)
		err = r.client.Update(ctx, tr)
		if err != nil {
			return reconcile.Result{}, err
		}

		secret := v12.Secret{}
		//delete the secret
		err = r.client.Get(ctx, types.NamespacedName{Namespace: tr.Namespace, Name: secretName}, &secret)
		if err == nil {
			log.Info("deleting secret from task")
			//PR is done, clean up the secret
			err := r.client.Delete(ctx, &secret)
			if err != nil {
				log.Error(err, "unable to delete secret")
			}
		} else if !errors.IsNotFound(err) {
			log.Error(err, "error deleting secret", "secret", secretName)
			return reconcile.Result{}, err
		} else {
			log.Info("could not find secret", "secret", secretName)
		}
		return r.handleWaitingTasks(ctx, log, platform)
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileTaskRun) readConfiguration(ctx context.Context, log *logr.Logger, targetPlatform string, targetNamespace string) (PlatformConfig, string, error) {
	cm := v12.ConfigMap{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.operatorNamespace, Name: HostConfig}, &cm)
	if err != nil {
		return nil, "", err
	}

	namespaces := cm.Data[AllowedNamespaces]
	if namespaces != "" {
		parts := strings.Split(namespaces, ",")
		ok := false
		for _, i := range parts {
			matchString, err := regexp.MatchString(i, targetNamespace)
			if err != nil {
				log.Error(err, "invalid allowed-namespace regex")
				continue
			}
			if matchString {
				ok = true
				break
			}
		}
		if !ok {
			return nil, "", fmt.Errorf("namespace %s does not match any namespace defined in allowed namespaces, ask an administrator to enable multi platform builds for your namespace", targetNamespace)
		}
	}

	dynamic := cm.Data[DynamicPlatforms]
	for _, platform := range strings.Split(dynamic, ",") {
		platformConfigName := strings.ReplaceAll(platform, "/", "-")
		if platform == targetPlatform {

			typeName := cm.Data["dynamic."+platformConfigName+".type"]
			allocfunc := r.cloudProviders[typeName]
			if allocfunc == nil {
				return nil, "", errors2.New("unknown dynamic provisioning type " + typeName)
			}
			maxInstances, err := strconv.Atoi(cm.Data["dynamic."+platformConfigName+".max-instances"])
			if err != nil {
				return nil, "", err
			}
			return DynamicResolver{
				CloudProvider: allocfunc(platformConfigName, cm.Data, r.operatorNamespace),
				SshSecret:     cm.Data["dynamic."+platformConfigName+".ssh-secret"],
				Platform:      platform,
				MaxInstances:  maxInstances,
			}, cm.Data["instance-tag"], nil

		}
	}

	ret := HostPool{hosts: map[string]*Host{}, targetPlatform: targetPlatform}
	for k, v := range cm.Data {
		if !strings.HasPrefix(k, "host.") {
			continue
		}
		k = k[len("host."):]
		pos := strings.LastIndex(k, ".")
		if pos == -1 {
			continue
		}
		name := k[0:pos]
		key := k[pos+1:]
		host := ret.hosts[name]
		if host == nil {
			host = &Host{}
			ret.hosts[name] = host
			host.Name = name
		}
		switch key {
		case "address":
			host.Address = v
		case "user":
			host.User = v
		case "platform":
			host.Platform = v
		case "secret":
			host.Secret = v
		case "concurrency":
			atoi, err := strconv.Atoi(v)
			if err != nil {
				return nil, "", err
			}
			host.Concurrency = atoi
		default:
			log.Info("unknown key", "key", key)
		}

	}
	return ret, cm.Data["instance-tag"], nil
}

type PlatformConfig interface {
	Allocate(r *ReconcileTaskRun, ctx context.Context, log *logr.Logger, tr *v1.TaskRun, secretName string, instanceTag string) (reconcile.Result, error)
	Deallocate(r *ReconcileTaskRun, ctx context.Context, log *logr.Logger, tr *v1.TaskRun, secretName string, selectedHost string) error
}

func launchProvisioningTask(r *ReconcileTaskRun, ctx context.Context, log *logr.Logger, tr *v1.TaskRun, secretName string, sshSecret string, address string, user string) error {
	//kick off the provisioning task
	//note that we can't use owner refs here because this task runs in a different namespace

	//first verify the secret exists, so we don't hang if it is missing
	secret := v12.Secret{}
	err := r.client.Get(ctx, types.NamespacedName{Namespace: r.operatorNamespace, Name: sshSecret}, &secret)
	if err != nil {
		log.Error(fmt.Errorf("failed to find SSH secret %s", sshSecret), "failed to find SSH secret")
		return r.createErrorSecret(ctx, tr, secretName, "failed to get SSH secret, system may not be configured correctly")
	}

	provision := v1.TaskRun{}
	provision.GenerateName = "provision-task"
	provision.Namespace = r.operatorNamespace
	provision.Labels = map[string]string{TaskTypeLabel: TaskTypeProvision, UserTaskNamespace: tr.Namespace, UserTaskName: tr.Name, AssignedHost: tr.Labels[AssignedHost]}
	provision.Spec.TaskRef = &v1.TaskRef{Name: "provision-shared-host"}
	provision.Spec.Workspaces = []v1.WorkspaceBinding{{Name: "ssh", Secret: &v12.SecretVolumeSource{SecretName: sshSecret}}}
	provision.Spec.ServiceAccountName = ServiceAccountName //TODO: special service account for this
	provision.Spec.Params = []v1.Param{
		{
			Name:  "SECRET_NAME",
			Value: *v1.NewStructuredValues(secretName),
		},
		{
			Name:  "TASKRUN_NAME",
			Value: *v1.NewStructuredValues(tr.Name),
		},
		{
			Name:  "NAMESPACE",
			Value: *v1.NewStructuredValues(tr.Namespace),
		},
		{
			Name:  "HOST",
			Value: *v1.NewStructuredValues(address),
		},
		{
			Name:  "USER",
			Value: *v1.NewStructuredValues(user),
		},
	}

	err = r.client.Create(ctx, &provision)
	return err
}

type Host struct {
	Address     string
	Name        string
	User        string
	Concurrency int
	Platform    string
	Secret      string
}

func platformLabel(platform string) string {
	return strings.ReplaceAll(platform, "/", "-")
}
