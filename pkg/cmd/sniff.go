package cmd

import (
	"fmt"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"io"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd/api"
	"ksniff/kube"
	"os"
	"os/exec"
	"time"
)

var (
	ksniffExample = "kubectl sniff hello-minikube-7c77b68cff-qbvsd -c hello-minikube"
)

const tcpdumpLocalPath = "/tcpdump-static"
const tcpdumpRemotePath = "/tmp/static-tcpdump"

type SniffOptions struct {
	configFlags                    *genericclioptions.ConfigFlags
	resultingContext               *api.Context
	userSpecifiedPod               string
	userSpecifiedFilter            string
	userSpecifiedContainer         string
	userSpecifiedNamespace         string
	userSpecifiedOutputFile        string
	userSpecifiedLocalTcpdumpPath  string
	userSpecifiedRemoteTcpdumpPath string
	clientset                      *kubernetes.Clientset
	restConfig                     *rest.Config
	rawConfig                      api.Config
	genericclioptions.IOStreams
}

func NewSniffOptions(streams genericclioptions.IOStreams) *SniffOptions {
	return &SniffOptions{
		configFlags: genericclioptions.NewConfigFlags(),

		IOStreams: streams,
	}
}

func NewCmdSniff(streams genericclioptions.IOStreams) *cobra.Command {
	o := NewSniffOptions(streams)

	cmd := &cobra.Command{
		Use:          "sniff pod [-n namespace] [-c container] [-f filter] [-o output_file]",
		Short:        "Perform network sniffing on a container running in a kubernetes cluster.",
		Example:      ksniffExample,
		SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Complete(c, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			if err := o.Run(); err != nil {
				return err
			}

			return nil
		},
	}

	cmd.Flags().StringVarP(&o.userSpecifiedNamespace, "namespace", "n", "default", "namespace (optional)")
	cmd.Flags().StringVarP(&o.userSpecifiedContainer, "container", "c", "", "container (optional)")
	cmd.Flags().StringVarP(&o.userSpecifiedFilter, "filter", "f", "", "filter (optional)")
	cmd.Flags().StringVarP(&o.userSpecifiedOutputFile, "output_file", "o", "", "output file path, tcpdump output will be redirect to this file instead of wireshark (optional)")
	cmd.Flags().StringVarP(&o.userSpecifiedLocalTcpdumpPath, "local_tcpdump_path", "l", tcpdumpLocalPath, "local static tcpdump binary path (optional)")
	cmd.Flags().StringVarP(&o.userSpecifiedRemoteTcpdumpPath, "remote_tcpdump_path", "r", tcpdumpRemotePath, "remote static tcpdump binary path (optional)")

	return cmd
}

func (o *SniffOptions) Complete(cmd *cobra.Command, args []string) error {

	if len(args) < 1 {
		cmd.Usage()
		return errors.Errorf("not enough arguments (%d)", len(args))
	}

	o.userSpecifiedPod = args[0]

	var err error

	o.rawConfig, err = o.configFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return err
	}

	o.restConfig, err = o.configFlags.ToRESTConfig()
	if err != nil {
		return err
	}

	o.restConfig.Timeout = 30 * time.Second

	o.clientset, err = kubernetes.NewForConfig(o.restConfig)
	if err != nil {
		return err
	}

	currentContext, exists := o.rawConfig.Contexts[o.rawConfig.CurrentContext]
	if !exists {
		return errors.New("context doesn't exist")
	}

	o.resultingContext = currentContext.DeepCopy()
	o.resultingContext.Namespace = o.userSpecifiedNamespace

	return nil
}

func (o *SniffOptions) Validate() error {
	if len(o.rawConfig.CurrentContext) == 0 {
		return errors.New("context doesn't exist")
	}

	if o.userSpecifiedNamespace == "" {
		return errors.New("namespace value is empty should be custom or default")
	}

	pod, err := o.clientset.CoreV1().Pods(o.userSpecifiedNamespace).Get(o.userSpecifiedPod, v1.GetOptions{})
	if err != nil {
		return err
	}

	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return errors.Errorf("cannot sniff on a container in a completed pod; current phase is %s", pod.Status.Phase)
	}

	if len(pod.Spec.Containers) < 1 {
		return errors.New("no containers in specified pod")
	}

	if o.userSpecifiedContainer == "" {
		log.Info("no container specified, taking first container we found in pod.")
		o.userSpecifiedContainer = pod.Spec.Containers[0].Name
		log.Infof("selected container: '%s'", o.userSpecifiedContainer)
	}

	return nil
}

func CheckIfTcpdumpExistOnPod(o *SniffOptions, tcpdumpRemotePath string) (bool, error) {
	stdOut := new(kube.Writer)
	stdErr := new(kube.Writer)

	req := kube.ExecCommandRequest{
		KubeRequest: kube.KubeRequest{
			Clientset:  o.clientset,
			RestConfig: o.restConfig,
			Namespace:  o.userSpecifiedNamespace,
			Pod:        o.userSpecifiedPod,
			Container:  o.userSpecifiedContainer,
		},
		Command: []string{"/bin/sh", "-c", fmt.Sprintf("ls -alt %s", tcpdumpRemotePath)},
		StdOut:  stdOut,
		StdErr:  stdErr,
	}

	exitCode, err := kube.PodExecuteCommand(req)
	if err != nil {
		return false, err
	}

	if exitCode != 0 {
		return false, nil
	}

	if stdErr.Output != "" {
		return false, errors.New("failed to check for tcpdump")
	}

	log.Infof("static-tcpdump found: %s", stdOut.Output)

	return true, nil
}

func (o *SniffOptions) UploadTcpdumpIfMissing() error {
	log.Infof("checking for static tcpdump binary on: %s", o.userSpecifiedRemoteTcpdumpPath)

	isExist, err := CheckIfTcpdumpExistOnPod(o, o.userSpecifiedRemoteTcpdumpPath)
	if err != nil {
		return err
	}

	if isExist {
		log.Info("tcpdump was already on remote pod")
		return nil
	}

	log.Infof("couldn't find static tcpdump binary on: %s, starting to upload", o.userSpecifiedRemoteTcpdumpPath)

	req := kube.UploadFileRequest{
		KubeRequest: kube.KubeRequest{
			Clientset:  o.clientset,
			RestConfig: o.restConfig,
			Namespace:  o.userSpecifiedNamespace,
			Pod:        o.userSpecifiedPod,
			Container:  o.userSpecifiedContainer,
		},
		Src: o.userSpecifiedLocalTcpdumpPath,
		Dst: o.userSpecifiedRemoteTcpdumpPath,
	}

	exitCode, err := kube.PodUploadFile(req)
	if err != nil || exitCode != 0 {
		return errors.Wrapf(err, "upload file command failed, exitCode: %d", exitCode)
	}

	log.Info("tcpdump uploaded successfully")

	return nil
}

func (o *SniffOptions) ExecuteTcpdumpOnRemotePod(stdOut io.Writer) {

	stdErr := new(kube.NopWriter)

	executeTcpdumpRequest := kube.ExecCommandRequest{
		KubeRequest: kube.KubeRequest{
			Clientset:  o.clientset,
			RestConfig: o.restConfig,
			Namespace:  o.userSpecifiedNamespace,
			Pod:        o.userSpecifiedPod,
			Container:  o.userSpecifiedContainer,
		},
		Command: []string{o.userSpecifiedRemoteTcpdumpPath, "-U", "-w", "-"},
		StdErr:  stdErr,
		StdOut:  stdOut,
	}

	kube.PodExecuteCommand(executeTcpdumpRequest)
}

func (o *SniffOptions) Run() error {

	log.Info("Starting ksniff")
	defer log.Info("Ksniff done")

	log.Infof("Sniffing on pod: '%s' [namespace: '%s', container: '%s', filter: '%s']",
		o.userSpecifiedPod, o.userSpecifiedNamespace, o.userSpecifiedContainer, o.userSpecifiedFilter)

	err := o.UploadTcpdumpIfMissing()
	if err != nil {
		return err
	}

	var outputWriter io.Writer

	if o.userSpecifiedOutputFile != "" {
		f, err := os.Create(o.userSpecifiedOutputFile)
		if err != nil {
			return err
		}

		o.ExecuteTcpdumpOnRemotePod(f)

	} else {
		cmd := exec.Command("wireshark", "-k", "-i", "-")

		outputWriter, err = cmd.StdinPipe()
		if err != nil {
			return err
		}

		go o.ExecuteTcpdumpOnRemotePod(outputWriter)

		err = cmd.Run()
		if err != nil {
			return err
		}
	}

	return nil
}
