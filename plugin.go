package main

import (
	"io"
	"os"
	"reflect"
	"strings"

	"errors"
	"fmt"
	"sync"

	"github.com/sirupsen/logrus"
	"k8s.io/api/batch/v1"
	coreV1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Plugin struct represents the data available for the plugin's logic.
type Plugin struct {
	JobName          string
	Namespace        string
	Image            string
	Workspace        string
	WorkspacePVC     string
	ServiceAccount   string
	OriginalCommands []string
	LabelSelector    map[string]string
	Env              map[string]string
	Wg               *sync.WaitGroup
}

const (
	JobWatcherStatusKey = "job"
	PodWatcherStatusKey = "pod"
	LogWatcherStatusKey = "log"

	pluginEnvPrefix = "PLUGIN_"
	droneEnvPrefix  = "DRONE_"
)

var (
	// the period before a resource (job, pvc) gets deleted
	gracePeriodSeconds = int64(2)
)

func init() {
	logrus.SetOutput(os.Stdout)
	logrus.SetLevel(logrus.InfoLevel)
}

func watchingStatusOn(watcherStatusKey string) {
	logrus.Debugf("Switching on logging status for: [ %s ]", watcherStatusKey)
	watcherStatusMap[watcherStatusKey] = true
}

func watchingStatusOff(watcherStatusKey string) {
	logrus.Debugf("Switching off logging status for: [ %s ]", watcherStatusKey)
	watcherStatusMap[watcherStatusKey] = false
}

func watchingStatus(watcherStatusKey string) bool {
	return watcherStatusMap[watcherStatusKey]
}

var (
	// internal status of the watchers
	watcherStatusMap = map[string]bool{"job": false, "pod": false, "log": false}
)

func (p *Plugin) handleJobEvent(event watch.Event, watcher watch.Interface, clientSet *kubernetes.Clientset) error {

	payloadType := reflect.TypeOf(event.Object)
	payload := reflect.ValueOf(event.Object).Interface().(*v1.Job)
	logrus.Debugf("received JOB event with payload type [ %s ]", payloadType)

	switch event.Type {
	case watch.Added:
		logrus.Debugf("job added; name: [ %s ], status: [ %s ], ", payload.GetName(), payload.Status.String())
	case watch.Modified:
		logrus.Debugf("job modified, status: %s", payload.Status.String())

		if payload.Status.Failed > 0 {
			watcher.Stop()
			return errors.New(fmt.Sprintf("there are [ %d ] failed pods", payload.Status.Failed))
		}

		if payload.Status.Succeeded > 0 {
			// watcher stopped + nil == app is quitting
			watcher.Stop()
			return nil
		}

		if watchingStatus(PodWatcherStatusKey) == true {
			logrus.Debugf("pod is already being watched")
			return nil
		}

		podWatcher, err := p.WatchPod(clientSet)
		if err != nil {
			logrus.Errorf("could not watch pod")
			return err
		}

		p.Wg.Add(1)
		// new goroutine as it blocks
		go p.PodEvents(podWatcher, clientSet)

	case watch.Deleted:
		logrus.Debugf("job deleted; name: [ %s ]", payload.GetName())
		logrus.Debugf("closing the job watcher")
		watcher.Stop()
	case watch.Error:
		logrus.Debugf("job in error, status: [ %s ]", event.Object.GetObjectKind())
	default:
		logrus.Debugf("received (unhandled) event of type: [ %s ]", payloadType)
	}

	return nil

}

func (p *Plugin) handlePodEvent(event watch.Event, watcher watch.Interface, clientSet *kubernetes.Clientset) {

	payload := reflect.ValueOf(event.Object).Interface().(*coreV1.Pod)

	switch event.Type {
	case watch.Added:
		logrus.Debugf("pod [ %s ] added, phase: [ %s ]", payload.GetName(), payload.Status.Phase)

	case watch.Modified:
		logrus.Debugf("pod [ %s ] modified, phase: [ %s ]", payload.GetName(), payload.Status.Phase)

		if watchingStatus(LogWatcherStatusKey) == true {
			logrus.Debugf("logs already being watched")
			return
		}

		// new thread not to block here
		go p.WatchLogs(payload.GetName(), clientSet)
	case watch.Error:
		logrus.Debugf("pod in error, phase: [ %s ]", payload.Status.Phase)
	case watch.Deleted:
		logrus.Debugf("pod [ %s] deleted", payload.GetName())
		logrus.Debugf("closing the pod watcher")
		watcher.Stop()
		watchingStatusOff(PodWatcherStatusKey)
	default:
		logrus.Debugf("received (unhandled) event of type: [ %s ]", event.Type)
	}

}

// CreateJob creates and launches a Job resource on the k8s cluster
func (p *Plugin) CreateJob(clientSet *kubernetes.Clientset) error {
	jobToRun, err := p.assembleJob()
	if err != nil {
		logrus.Errorf("could not set up job. error: %s", err)
		return err
	}

	jobToRun, err = p.DecorateJob(jobToRun)
	if err != nil {
		logrus.Errorf("could not decorate job. error: %s", err)
		return err
	}

	job, err := clientSet.BatchV1().Jobs(p.Namespace).Create(jobToRun)
	if err != nil {
		logrus.Errorf("could not create job. error: %s", err)
		return err
	}

	logrus.Debugf("created job: [ %s ]", job.GetName())
	return nil
}

// DeleteJob deletes a job from the k8s cluster
func (p *Plugin) DeleteJob(clientSet *kubernetes.Clientset) error {

	deleteOptions := metaV1.DeleteOptions{GracePeriodSeconds: &gracePeriodSeconds}

	err := clientSet.BatchV1().Jobs(p.Namespace).Delete(p.JobName, &deleteOptions)
	if err != nil {
		return err
	}
	logrus.Debugf("deleted job: [ %s ]", p.JobName)
	return nil

}

// assembleJob builds the Job struct based on the plugin
func (p *Plugin) assembleJob() (*v1.Job, error) {

	falseVal := false

	batchJob := &v1.Job{
		TypeMeta: metaV1.TypeMeta{
			Kind:       "Job",
			APIVersion: "batch/v1",
		},
		ObjectMeta: metaV1.ObjectMeta{
			Name:   p.JobName,
			Labels: p.LabelSelector,
		},
		Spec: v1.JobSpec{
			Template: coreV1.PodTemplateSpec{
				ObjectMeta: metaV1.ObjectMeta{
					Name:   p.JobName,
					Labels: p.LabelSelector,
				},
				Spec: coreV1.PodSpec{
					ServiceAccountName: p.ServiceAccount,
					Containers: []coreV1.Container{
						{
							Name:       p.JobName,
							Image:      p.Image,
							WorkingDir: p.Workspace,
							SecurityContext: &coreV1.SecurityContext{
								Privileged: &falseVal,
							},
							ImagePullPolicy: coreV1.PullPolicy(coreV1.PullIfNotPresent),
							Env:             p.originalEnvVars(),
							VolumeMounts: []coreV1.VolumeMount{
								coreV1.VolumeMount{
									Name:      p.JobName,
									MountPath: p.Workspace,
								},
							},
						},
					},
					RestartPolicy: coreV1.RestartPolicyNever,
					Volumes: []coreV1.Volume{
						coreV1.Volume{
							Name: p.JobName,
							VolumeSource: coreV1.VolumeSource{
								PersistentVolumeClaim: &coreV1.PersistentVolumeClaimVolumeSource{
									ClaimName: p.WorkspacePVC,
								},
							},
						},
					},
					ImagePullSecrets: []coreV1.LocalObjectReference{},
				},
			},
		},
	}

	return batchJob, nil

}

func (p *Plugin) DecorateJob(job *v1.Job) (*v1.Job, error) {

	if p.OriginalCommands != nil && len(p.OriginalCommands) > 0 {
		// we assume there is a single container only in the job/pod specification
		container := &job.Spec.Template.Spec.Containers[0]
		container.Command = []string{"sh", "-c"}
		container.Args = p.OriginalCommands
		logrus.Debugf("set original command: [ %s ] with argument(s): [ %s ]", container.Command, container.Args)
	}
	return job, nil
}

func (p *Plugin) WatchLogs(podName string, clientSet *kubernetes.Clientset) {

	logOptions := coreV1.PodLogOptions{
		Follow: true,
	}
	req := clientSet.CoreV1().Pods(p.Namespace).GetLogs(podName, &logOptions)

	readCloser, err := req.Stream()
	if err != nil {
		logrus.Debugf("could not stream the logs. error: %s", err)
		watchingStatusOff(LogWatcherStatusKey)
		return
	}

	//close the readcloser on exiting this method
	defer readCloser.Close()

	logrus.Infof("***** streaming the logs for pod [ %s ] *****", podName)
	watchingStatusOn(LogWatcherStatusKey)

	// this is blocking till logs are written
	written, err := io.Copy(os.Stdout, readCloser)

	logrus.Debugf("Bytes written: [ %s ]. error: [ %s ]. ", written, err)
	watchingStatusOff(LogWatcherStatusKey)
	logrus.Infof("***** end of the logs for pod [ %s ] *****", podName)
	// regardless the result of copy the goroutine ends here, need to signal it
	p.Wg.Done()

}

func (p *Plugin) WatchJob(clientSet *kubernetes.Clientset) (watch.Interface, error) {

	// set up the proper list options, use labels
	options := metaV1.ListOptions{
		Watch:         true,
		LabelSelector: strings.Join([]string{label, p.LabelSelector[label]}, "="),
	}

	jobWatcher, err := clientSet.BatchV1().Jobs(p.Namespace).Watch(options)
	if err != nil {
		logrus.Errorf("could not watch jobs. err: %s", err)
		watchingStatusOff(JobWatcherStatusKey)
		return nil, err
	}
	watchingStatusOn(JobWatcherStatusKey)
	logrus.Debugf("job watcher started")
	return jobWatcher, nil

}

func (p *Plugin) WatchPod(clientSet *kubernetes.Clientset) (watch.Interface, error) {

	// set up the proper list options, use labels
	options := metaV1.ListOptions{
		LabelSelector: strings.Join([]string{label, p.LabelSelector[label]}, "="),
	}

	// at his point we don't know the name of the pod
	podWatcher, err := clientSet.CoreV1().Pods(p.Namespace).Watch(options)
	if err != nil {
		logrus.Errorf("could not watch pod. err: %s", err)
		watchingStatusOff(PodWatcherStatusKey)
		return nil, err
	}

	watchingStatusOn(PodWatcherStatusKey)
	logrus.Debugf("pod watcher started")
	return podWatcher, nil

}

// JobEvents handles job related events. Blocks till watcher is closed
func (p *Plugin) JobEvents(watcher watch.Interface, clientSet *kubernetes.Clientset) error {
	for event := range watcher.ResultChan() {
		err := p.handleJobEvent(event, watcher, clientSet)
		if err != nil {
			return err
		}
	}
	logrus.Debugf("job [%s] succeeded", p.JobName)
	// wait till the log reader goroutine is done
	return nil
}

// PodEvents handles pod related events. Blocks till watcher is closed
func (p *Plugin) PodEvents(watcher watch.Interface, clientSet *kubernetes.Clientset) {
	for event := range watcher.ResultChan() {
		p.handlePodEvent(event, watcher, clientSet)
	}
}

// OriginalEnvVars processes the environment passed to the job (selects specially prefixed env vars)
func (p *Plugin) originalEnvVars() []coreV1.EnvVar {
	originalEnv := make([]coreV1.EnvVar, 0)
	for key, val := range p.Env {
		if strings.HasPrefix(key, pluginEnvPrefix) || strings.HasPrefix(key, droneEnvPrefix) {
			originalEnv = append(originalEnv, coreV1.EnvVar{
				Name:  key,
				Value: val,
			})
		}
	}
	logrus.Debugf("original env passed to the job: %#v", originalEnv)
	return originalEnv
}

// CreateOrGetPVC creates a persistent volume claim resource in case it doesn't already exist
func (p *Plugin) CreateOrGetPVC(clientSet *kubernetes.Clientset) (*coreV1.PersistentVolumeClaim, error) {

	claim, err := clientSet.CoreV1().PersistentVolumeClaims(p.Namespace).Get(p.WorkspacePVC, metaV1.GetOptions{})
	if err != nil {
		logrus.Warnf("error while getting the PVC: [ %s ], error %s;", p.WorkspacePVC, err)
	} else {
		logrus.Debugf("using existing PVC: [ %s ]", claim.String())
		return claim, nil
	}

	pvc := coreV1.PersistentVolumeClaim{
		ObjectMeta: metaV1.ObjectMeta{
			Name:   p.WorkspacePVC,
			Labels: p.LabelSelector,
		},
		Spec: coreV1.PersistentVolumeClaimSpec{
			AccessModes: []coreV1.PersistentVolumeAccessMode{coreV1.ReadWriteOnce},
			Resources: coreV1.ResourceRequirements{
				Requests: map[coreV1.ResourceName]resource.Quantity{
					coreV1.ResourceStorage: resource.MustParse("3Gi"),
				},
			},
		},
	}

	claim, err = clientSet.CoreV1().PersistentVolumeClaims(p.Namespace).Create(&pvc)
	if err != nil {
		logrus.Errorf("could not create PVC, error %s", err)
		return nil, err
	}
	logrus.Debugf("created PVC: [ %s ]", claim.GetName())
	return claim, nil
}

// DeletePVC deletes a persistent volume claim resource
func (p *Plugin) DeletePVC(clientSet *kubernetes.Clientset) error {
	deleteOptions := metaV1.DeleteOptions{
		GracePeriodSeconds: &gracePeriodSeconds,
	}

	err := clientSet.CoreV1().PersistentVolumeClaims(p.Namespace).Delete(p.WorkspacePVC, &deleteOptions)
	if err != nil {
		logrus.Errorf("could not delete pvc:[ %s ], error: %s", p.WorkspacePVC, err)
		return err
	}
	logrus.Debugf("deleted PVC: [ %s ]", p.WorkspacePVC)
	return nil
}

func (p *Plugin) Cleanup(clientSet *kubernetes.Clientset) {
	p.DeleteJob(clientSet)
	//p.DeletePVC(clientSet)
}
