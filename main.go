package main

import (
	"flag"

	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"path/filepath"
	"sync"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	label      = "label-name"
	appName    = "k8s client plugin"
	appVersion = "0.0.1"
)

var (
	labels = map[string]string{label: "labelValue"}
)

func main() {

	app := cli.NewApp()
	app.Name = appName
	app.Usage = ""
	app.Action = run
	app.Version = fmt.Sprintf("%s", appVersion)
	app.EnableBashCompletion = true

	app.Flags = []cli.Flag{

		cli.StringFlag{
			Name:   "plugin.job.namespace",
			Usage:  "the namespace of the job",
			EnvVar: "PLUGIN_JOB_NAMESPACE",
			Value:  "default",
		},
		cli.StringFlag{
			Name:   "plugin.original.image",
			Usage:  "the image to ebe run on the cluster",
			EnvVar: "PLUGIN_ORIGINAL_IMAGE",
		},
		cli.StringFlag{
			Name:   "plugin.proxy.service.account",
			Usage:  "the service account name",
			EnvVar: "PLUGIN_PROXY_SERVICE_ACCOUNT",
			Value:  "default",
		},
		cli.StringFlag{
			Name:   "plugin.job.workspace",
			Usage:  "repository full name",
			EnvVar: "PLUGIN_JOB_WORKSPACE",
		},

		cli.StringFlag{
			Name:   "plugin.job.label.selector",
			Usage:  "repository full name",
			EnvVar: "PLUGIN_JOB_LABEL_SELECTOR",
		},
	}

	err := app.Run(os.Args)
	if err != nil {
		logrus.Errorf("plugin execution failed. error: %s", err)
		os.Exit(1)
	}

}

func run(c *cli.Context) error {

	logrus.Debugf("plugin environment: %s", os.Environ())
	flag.Parse()

	config, err := clientcmd.BuildConfigFromFlags("", kubeConfigPath())
	if err != nil {
		logrus.Errorf("could not build kubeconfig. err: %s", err)
		return err
	}
	clientSet, err := kubernetes.NewForConfig(config)

	if err != nil {
		logrus.Errorf("could not get client set  %s", err)
		return err
	}

	if err != nil {
		logrus.Errorf("could not read env  %s", err)
		return err
	}
	var wg sync.WaitGroup

	plugin := Plugin{
		Namespace:        c.String("plugin.job.namespace"),
		Image:            c.String("plugin.original.image"),
		ServiceAccount:   c.String("plugin.proxy.service.account"),
		Workspace:        workspace(),
		WorkspacePVC:     workspacePVC(),
		JobName:          jobName(),
		OriginalCommands: originalCommands(),
		LabelSelector:    labels,
		Env:              pluginEnv(),
		Wg:               &wg,
	}

	_, err = plugin.CreateOrGetPVC(clientSet)
	if err != nil {
		logrus.Errorf("could not create PVC. err [ %s ]", err)
		return err
	}

	jobWatcher, err := plugin.WatchJob(clientSet)
	if err != nil {
		logrus.Errorf("could not watch jobs. err [ %s ]", err)
		return err
	}

	err = plugin.CreateJob(clientSet)
	if err != nil {
		jobWatcher.Stop()
		return err
	}

	err = plugin.JobEvents(jobWatcher, clientSet)
	if err != nil {
		logrus.Errorf("error encountered: %s", err)
		return err
	}

	plugin.Wg.Wait()
	plugin.Cleanup(clientSet)

	return nil

}

// WorkspacePVC assembles the name of the persistent volume claim based on the available environment
func workspacePVC() string {
	//DRONE_WORKSPACE_PVC=$DRONE_REPO_NAME"-"$DRONE_BUILD_NUMBER"-WORKSPACE"
	// PVC must be lowercase!
	workSpacePVC := strings.ToLower(strings.Join([]string{os.Getenv("DRONE_REPO_NAME"),
		os.Getenv("DRONE_BUILD_NUMBER"), "WORKSPACE"}, "-"))

	logrus.Debugf("workspace PVC name: [ %s ]", workSpacePVC)
	return workSpacePVC
}

func workspace() string {
	ws := os.Getenv("DRONE_WORKSPACE")
	logrus.Debugf("workspace: [ %s ]", ws)
	return ws
}

// JobName assembles the name of the job based on the available environment
func jobName() string {
	//DRONE_JOB_NAME=$DRONE_REPO_NAME"-"$DRONE_BUILD_NUMBER-`date +%s`
	jobName := strings.Join([]string{os.Getenv("DRONE_REPO_NAME"), os.Getenv("DRONE_BUILD_NUMBER"),
		strconv.FormatInt(time.Now().Unix(), 10)}, "-")
	logrus.Debugf("job name: [ %s ]", jobName)
	return jobName
}

// OriginalCommands parses the passed in original command
// The original command is passed to the containar "as is"
func originalCommands() []string {
	oc, ok := os.LookupEnv("PLUGIN_ORIGINAL_COMMANDS")
	if ok == false || oc == "" {
		return nil
	}
	logrus.Debugf("original commands: [ %s ]", oc)
	return []string{oc}
}

// KubeConfigPath assembles the path to the Kubernetes config file
func kubeConfigPath() string {
	//export KUBECONFIG="$DRONE_WORKSPACE/.kube/config"
	kubeConfigPath := filepath.Join(os.Getenv("DRONE_WORKSPACE"), ".kube", "config")
	logrus.Infof("kube config path: [ %s ]", kubeConfigPath)
	return kubeConfigPath
}

func pluginEnv() map[string]string {
	pluginEnv := map[string]string{}
	for _, envVar := range os.Environ() {
		keyVal := strings.SplitN(envVar, "=", 2)
		pluginEnv[keyVal[0]] = keyVal[1]
	}
	logrus.Debugf("parsed env map: %s", pluginEnv)
	return pluginEnv
}
