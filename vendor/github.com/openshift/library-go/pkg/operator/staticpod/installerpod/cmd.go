package installerpod

import (
	"context"
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"time"

	"github.com/davecgh/go-spew/spew"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/klog/v2"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/openshift/library-go/pkg/config/client"
	"github.com/openshift/library-go/pkg/operator/events"
	"github.com/openshift/library-go/pkg/operator/resource/resourceread"
	"github.com/openshift/library-go/pkg/operator/resource/retry"
	"github.com/openshift/library-go/pkg/operator/staticpod"
	"github.com/openshift/library-go/pkg/operator/staticpod/internal/flock"
)

type InstallOptions struct {
	// TODO replace with genericclioptions
	KubeConfig string
	KubeClient kubernetes.Interface

	Revision  string
	NodeName  string
	Namespace string

	PodConfigMapNamePrefix        string
	SecretNamePrefixes            []string
	OptionalSecretNamePrefixes    []string
	ConfigMapNamePrefixes         []string
	OptionalConfigMapNamePrefixes []string

	CertSecretNames                   []string
	OptionalCertSecretNamePrefixes    []string
	CertConfigMapNamePrefixes         []string
	OptionalCertConfigMapNamePrefixes []string

	CertDir        string
	ResourceDir    string
	PodManifestDir string

	Timeout time.Duration

	// StaticPodManifestsLockFile used to coordinate work between multiple processes when writing static pod manifests
	StaticPodManifestsLockFile string

	PodMutationFns []PodMutationFunc
}

// PodMutationFunc is a function that has a chance at changing the pod before it is created
type PodMutationFunc func(pod *corev1.Pod) error

func NewInstallOptions() *InstallOptions {
	return &InstallOptions{}
}

func (o *InstallOptions) WithPodMutationFn(podMutationFn PodMutationFunc) *InstallOptions {
	o.PodMutationFns = append(o.PodMutationFns, podMutationFn)
	return o
}

func NewInstaller() *cobra.Command {
	o := NewInstallOptions()

	cmd := &cobra.Command{
		Use:   "installer",
		Short: "Install static pod and related resources",
		Run: func(cmd *cobra.Command, args []string) {
			klog.V(1).Info(cmd.Flags())
			klog.V(1).Info(spew.Sdump(o))

			if err := o.Complete(); err != nil {
				klog.Exit(err)
			}
			if err := o.Validate(); err != nil {
				klog.Exit(err)
			}

			ctx, cancel := context.WithTimeout(context.TODO(), o.Timeout)
			defer cancel()
			if err := o.Run(ctx); err != nil {
				klog.Exit(err)
			}
		},
	}

	o.AddFlags(cmd.Flags())

	return cmd
}

func (o *InstallOptions) AddFlags(fs *pflag.FlagSet) {
	fs.StringVar(&o.KubeConfig, "kubeconfig", o.KubeConfig, "kubeconfig file or empty")
	fs.StringVar(&o.Revision, "revision", o.Revision, "identifier for this particular installation instance.  For example, a counter or a hash")
	fs.StringVar(&o.Namespace, "namespace", o.Namespace, "namespace to retrieve all resources from and create the static pod in")
	fs.StringVar(&o.PodConfigMapNamePrefix, "pod", o.PodConfigMapNamePrefix, "name of configmap that contains the pod to be created")
	fs.StringSliceVar(&o.SecretNamePrefixes, "secrets", o.SecretNamePrefixes, "list of secret names to be included")
	fs.StringSliceVar(&o.ConfigMapNamePrefixes, "configmaps", o.ConfigMapNamePrefixes, "list of configmaps to be included")
	fs.StringSliceVar(&o.OptionalSecretNamePrefixes, "optional-secrets", o.OptionalSecretNamePrefixes, "list of optional secret names to be included")
	fs.StringSliceVar(&o.OptionalConfigMapNamePrefixes, "optional-configmaps", o.OptionalConfigMapNamePrefixes, "list of optional configmaps to be included")
	fs.StringVar(&o.ResourceDir, "resource-dir", o.ResourceDir, "directory for all files supporting the static pod manifest")
	fs.StringVar(&o.PodManifestDir, "pod-manifest-dir", o.PodManifestDir, "directory for the static pod manifest")
	fs.DurationVar(&o.Timeout, "timeout-duration", 120*time.Second, "maximum time in seconds to wait for the copying to complete (default: 2m)")
	fs.StringVar(&o.StaticPodManifestsLockFile, "pod-manifests-lock-file", o.StaticPodManifestsLockFile, "path to a file that will be used to coordinate writing static pod manifests between multiple processes")

	fs.StringSliceVar(&o.CertSecretNames, "cert-secrets", o.CertSecretNames, "list of secret names to be included")
	fs.StringSliceVar(&o.CertConfigMapNamePrefixes, "cert-configmaps", o.CertConfigMapNamePrefixes, "list of configmaps to be included")
	fs.StringSliceVar(&o.OptionalCertSecretNamePrefixes, "optional-cert-secrets", o.OptionalCertSecretNamePrefixes, "list of optional secret names to be included")
	fs.StringSliceVar(&o.OptionalCertConfigMapNamePrefixes, "optional-cert-configmaps", o.OptionalCertConfigMapNamePrefixes, "list of optional configmaps to be included")
	fs.StringVar(&o.CertDir, "cert-dir", o.CertDir, "directory for all certs")
}

func (o *InstallOptions) Complete() error {
	clientConfig, err := client.GetKubeConfigOrInClusterConfig(o.KubeConfig, nil)
	if err != nil {
		return err
	}

	// Use protobuf to fetch configmaps and secrets and create pods.
	protoConfig := rest.CopyConfig(clientConfig)
	protoConfig.AcceptContentTypes = "application/vnd.kubernetes.protobuf,application/json"
	protoConfig.ContentType = "application/vnd.kubernetes.protobuf"

	o.KubeClient, err = kubernetes.NewForConfig(protoConfig)
	if err != nil {
		return err
	}

	// set via downward API
	o.NodeName = os.Getenv("NODE_NAME")

	return nil
}

func (o *InstallOptions) Validate() error {
	if len(o.Revision) == 0 {
		return fmt.Errorf("--revision is required")
	}
	if len(o.NodeName) == 0 {
		return fmt.Errorf("env var NODE_NAME is required")
	}
	if len(o.Namespace) == 0 {
		return fmt.Errorf("--namespace is required")
	}
	if len(o.PodConfigMapNamePrefix) == 0 {
		return fmt.Errorf("--pod is required")
	}
	if len(o.ConfigMapNamePrefixes) == 0 {
		return fmt.Errorf("--configmaps is required")
	}
	if o.Timeout == 0 {
		return fmt.Errorf("--timeout-duration cannot be 0")
	}

	if o.KubeClient == nil {
		return fmt.Errorf("missing client")
	}

	return nil
}

func (o *InstallOptions) nameFor(prefix string) string {
	return fmt.Sprintf("%s-%s", prefix, o.Revision)
}

func (o *InstallOptions) prefixFor(name string) string {
	return name[0 : len(name)-len(fmt.Sprintf("-%s", o.Revision))]
}

func (o *InstallOptions) copySecretsAndConfigMaps(ctx context.Context, resourceDir string,
	secretNames, optionalSecretNames, configNames, optionalConfigNames sets.String, prefixed bool) error {
	klog.Infof("Creating target resource directory %q ...", resourceDir)
	if err := os.MkdirAll(resourceDir, 0755); err != nil && !os.IsExist(err) {
		return err
	}

	// Gather secrets. If we get API server error, retry getting until we hit the timeout.
	// Retrying will prevent temporary API server blips or networking issues.
	// We return when all "required" secrets are gathered, optional secrets are not checked.
	klog.Infof("Getting secrets ...")
	secrets := []*corev1.Secret{}
	for _, name := range append(secretNames.List(), optionalSecretNames.List()...) {
		secret, err := o.getSecretWithRetry(ctx, name, optionalSecretNames.Has(name))
		if err != nil {
			return err
		}
		// secret is nil means the secret was optional and we failed to get it.
		if secret != nil {
			secrets = append(secrets, o.substituteSecret(secret))
		}
	}

	klog.Infof("Getting config maps ...")
	configs := []*corev1.ConfigMap{}
	for _, name := range append(configNames.List(), optionalConfigNames.List()...) {
		config, err := o.getConfigMapWithRetry(ctx, name, optionalConfigNames.Has(name))
		if err != nil {
			return err
		}
		// config is nil means the config was optional and we failed to get it.
		if config != nil {
			configs = append(configs, o.substituteConfigMap(config))
		}
	}

	for _, secret := range secrets {
		secretBaseName := secret.Name
		if prefixed {
			secretBaseName = o.prefixFor(secret.Name)
		}
		contentDir := path.Join(resourceDir, "secrets", secretBaseName)
		klog.Infof("Creating directory %q ...", contentDir)
		if err := os.MkdirAll(contentDir, 0755); err != nil {
			return err
		}
		for filename, content := range secret.Data {
			if err := writeSecret(content, path.Join(contentDir, filename)); err != nil {
				return err
			}
		}
	}
	for _, configmap := range configs {
		configMapBaseName := configmap.Name
		if prefixed {
			configMapBaseName = o.prefixFor(configmap.Name)
		}
		contentDir := path.Join(resourceDir, "configmaps", configMapBaseName)
		klog.Infof("Creating directory %q ...", contentDir)
		if err := os.MkdirAll(contentDir, 0755); err != nil {
			return err
		}
		for filename, content := range configmap.Data {
			if err := writeConfig([]byte(content), path.Join(contentDir, filename)); err != nil {
				return err
			}
		}
	}

	return nil
}

func (o *InstallOptions) copyContent(ctx context.Context) error {
	resourceDir := path.Join(o.ResourceDir, o.nameFor(o.PodConfigMapNamePrefix))
	klog.Infof("Creating target resource directory %q ...", resourceDir)
	if err := os.MkdirAll(resourceDir, 0755); err != nil && !os.IsExist(err) {
		return err
	}

	secretPrefixes := sets.NewString()
	optionalSecretPrefixes := sets.NewString()
	configPrefixes := sets.NewString()
	optionalConfigPrefixes := sets.NewString()
	for _, prefix := range o.SecretNamePrefixes {
		secretPrefixes.Insert(o.nameFor(prefix))
	}
	for _, prefix := range o.OptionalSecretNamePrefixes {
		optionalSecretPrefixes.Insert(o.nameFor(prefix))
	}
	for _, prefix := range o.ConfigMapNamePrefixes {
		configPrefixes.Insert(o.nameFor(prefix))
	}
	for _, prefix := range o.OptionalConfigMapNamePrefixes {
		optionalConfigPrefixes.Insert(o.nameFor(prefix))
	}
	if err := o.copySecretsAndConfigMaps(ctx, resourceDir, secretPrefixes, optionalSecretPrefixes, configPrefixes, optionalConfigPrefixes, true); err != nil {
		return err
	}

	// Copy the current state of the certs as we see them.  This primes us once and allows a kube-apiserver to start once
	if len(o.CertDir) > 0 {
		if err := o.copySecretsAndConfigMaps(ctx, o.CertDir,
			sets.NewString(o.CertSecretNames...),
			sets.NewString(o.OptionalCertSecretNamePrefixes...),
			sets.NewString(o.CertConfigMapNamePrefixes...),
			sets.NewString(o.OptionalCertConfigMapNamePrefixes...),
			false,
		); err != nil {
			return err
		}
	}

	// Gather the config map that holds pods to be installed
	var podsConfigMap *corev1.ConfigMap

	err := retry.RetryOnConnectionErrors(ctx, func(ctx context.Context) (bool, error) {
		klog.Infof("Getting pod configmaps/%s -n %s", o.nameFor(o.PodConfigMapNamePrefix), o.Namespace)
		podConfigMap, err := o.KubeClient.CoreV1().ConfigMaps(o.Namespace).Get(ctx, o.nameFor(o.PodConfigMapNamePrefix), metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if _, exists := podConfigMap.Data["pod.yaml"]; !exists {
			return true, fmt.Errorf("required 'pod.yaml' key does not exist in configmap")
		}
		podsConfigMap = o.substituteConfigMap(podConfigMap)
		return true, nil
	})
	if err != nil {
		return err
	}

	// at this point we know that the required key is present in the config map, just make sure the manifest dir actually exists
	klog.Infof("Creating directory for static pod manifest %q ...", o.PodManifestDir)
	if err := os.MkdirAll(o.PodManifestDir, 0755); err != nil {
		return err
	}

	// check to see if we need to acquire a file based lock to coordinate work.
	// since writing to disk is fast and we need to write at most 2 files it is okay to hold a lock here
	// note that in case of unplanned disaster the Linux kernel is going to release the lock when the process exits
	if len(o.StaticPodManifestsLockFile) > 0 {
		installerLock := flock.New(o.StaticPodManifestsLockFile)
		klog.Infof("acquiring an exclusive lock on a %s", o.StaticPodManifestsLockFile)
		if err := installerLock.Lock(ctx); err != nil {
			return fmt.Errorf("failed to acquire an exclusive lock on %s, due to %v", o.StaticPodManifestsLockFile, err)
		}
		defer installerLock.Unlock()
	}

	// then write the required pod and all optional
	// the key must be pod.yaml or has a -pod.yaml suffix to be considered
	for rawPodKey, rawPod := range podsConfigMap.Data {
		var manifestFileName = rawPodKey
		if manifestFileName == "pod.yaml" {
			// TODO: update kas-o to update the key to a fully qualified name
			manifestFileName = o.PodConfigMapNamePrefix + ".yaml"
		} else if !strings.HasSuffix(manifestFileName, "-pod.yaml") {
			continue
		}

		klog.Infof("Writing a pod under %q key \n%s", manifestFileName, rawPod)
		err := o.writePod([]byte(rawPod), manifestFileName, resourceDir)
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *InstallOptions) substituteConfigMap(obj *corev1.ConfigMap) *corev1.ConfigMap {
	ret := obj.DeepCopy()
	for k, oldContent := range obj.Data {
		newContent := strings.ReplaceAll(oldContent, "REVISION", o.Revision)
		newContent = strings.ReplaceAll(newContent, "NODE_NAME", o.NodeName)
		newContent = strings.ReplaceAll(newContent, "NODE_ENVVAR_NAME", strings.ReplaceAll(strings.ReplaceAll(o.NodeName, "-", "_"), ".", "_"))
		ret.Data[k] = newContent
	}
	return ret
}

func (o *InstallOptions) substituteSecret(obj *corev1.Secret) *corev1.Secret {
	ret := obj.DeepCopy()
	for k, oldContent := range obj.Data {
		newContent := strings.ReplaceAll(string(oldContent), "REVISION", o.Revision)
		newContent = strings.ReplaceAll(newContent, "NODE_NAME", o.NodeName)
		newContent = strings.ReplaceAll(newContent, "NODE_ENVVAR_NAME", strings.ReplaceAll(strings.ReplaceAll(o.NodeName, "-", "_"), ".", "_"))
		ret.Data[k] = []byte(newContent)
	}
	return ret
}

func (o *InstallOptions) Run(ctx context.Context) error {
	var eventTarget *corev1.ObjectReference

	err := retry.RetryOnConnectionErrors(ctx, func(context.Context) (bool, error) {
		var clientErr error
		eventTarget, clientErr = events.GetControllerReferenceForCurrentPod(o.KubeClient, o.Namespace, nil)
		if clientErr != nil {
			return false, clientErr
		}
		return true, nil
	})
	if err != nil {
		klog.Warningf("unable to get owner reference (falling back to namespace): %v", err)
	}

	recorder := events.NewRecorder(o.KubeClient.CoreV1().Events(o.Namespace), "static-pod-installer", eventTarget)
	if err := o.copyContent(ctx); err != nil {
		recorder.Warningf("StaticPodInstallerFailed", "Installing revision %s: %v", o.Revision, err)
		return fmt.Errorf("failed to copy: %v", err)
	}

	recorder.Eventf("StaticPodInstallerCompleted", "Successfully installed revision %s", o.Revision)
	return nil
}

func (o *InstallOptions) writePod(rawPodBytes []byte, manifestFileName, resourceDir string) error {
	// the kubelet has a bug that prevents graceful termination from working on static pods with the same name, filename
	// and uuid.  By setting the pod UID we can work around the kubelet bug and get our graceful termination honored.
	// Per the node team, this is hard to fix in the kubelet, though it will affect all static pods.
	pod, err := resourceread.ReadPodV1(rawPodBytes)
	if err != nil {
		return err
	}
	pod.UID = uuid.NewUUID()
	for _, fn := range o.PodMutationFns {
		klog.V(2).Infof("Customizing static pod ...")
		pod = pod.DeepCopy()
		if err := fn(pod); err != nil {
			return err
		}
	}
	finalPodBytes := resourceread.WritePodV1OrDie(pod)

	// Write secrets, config maps and pod to disk
	// This does not need timeout, instead we should fail hard when we are not able to write.
	klog.Infof("Writing pod manifest %q ...", path.Join(resourceDir, manifestFileName))
	if err := ioutil.WriteFile(path.Join(resourceDir, manifestFileName), []byte(finalPodBytes), 0644); err != nil {
		return err
	}

	// remove the existing file to ensure kubelet gets "create" event from inotify watchers
	if err := os.Remove(path.Join(o.PodManifestDir, manifestFileName)); err == nil {
		klog.Infof("Removed existing static pod manifest %q ...", path.Join(o.PodManifestDir, manifestFileName))
	} else if !os.IsNotExist(err) {
		return err
	}
	klog.Infof("Writing static pod manifest %q ...\n%s", path.Join(o.PodManifestDir, manifestFileName), finalPodBytes)
	if err := ioutil.WriteFile(path.Join(o.PodManifestDir, manifestFileName), []byte(finalPodBytes), 0644); err != nil {
		return err
	}
	return nil
}

func writeConfig(content []byte, fullFilename string) error {
	klog.Infof("Writing config file %q ...", fullFilename)

	filePerms := os.FileMode(0644)
	if strings.HasSuffix(fullFilename, ".sh") {
		filePerms = 0755
	}
	return staticpod.WriteFileAtomic(content, filePerms, fullFilename)
}

func writeSecret(content []byte, fullFilename string) error {
	klog.Infof("Writing secret manifest %q ...", fullFilename)

	filePerms := os.FileMode(0600)
	if strings.HasSuffix(fullFilename, ".sh") {
		filePerms = 0700
	}
	return staticpod.WriteFileAtomic(content, filePerms, fullFilename)
}
