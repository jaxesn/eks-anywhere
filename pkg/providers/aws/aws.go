package aws

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strconv"
	"time"

	"sigs.k8s.io/cluster-api/api/v1alpha3"
	kubeadmnv1alpha3 "sigs.k8s.io/cluster-api/controlplane/kubeadm/api/v1alpha3"

	"github.com/aws/eks-anywhere/pkg/api/v1alpha1"
	"github.com/aws/eks-anywhere/pkg/bootstrapper"
	"github.com/aws/eks-anywhere/pkg/cluster"
	"github.com/aws/eks-anywhere/pkg/clusterapi"
	"github.com/aws/eks-anywhere/pkg/executables"
	"github.com/aws/eks-anywhere/pkg/filewriter"
	"github.com/aws/eks-anywhere/pkg/logger"
	"github.com/aws/eks-anywhere/pkg/providers"
	"github.com/aws/eks-anywhere/pkg/templater"
	"github.com/aws/eks-anywhere/pkg/types"
	releasev1alpha1 "github.com/aws/eks-anywhere/release/api/v1alpha1"
)

const (
	ProviderName     = "aws"
	defaultAmiID     = "ami-04670a6600adbe545"
	defaultAWSRegion = "us-west-2"
)

//go:embed config/template.yaml
var defaultClusterConfig string

var requiredEnvVars = []string{
	"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_REGION",
	"AWS_SSH_KEY_NAME", "AWS_CONTROL_PLANE_MACHINE_TYPE", "AWS_NODE_MACHINE_TYPE", "AWS_SESSION_TOKEN", "GITHUB_TOKEN",
}

var eksaAwsResourceType = fmt.Sprintf("awsdatacenterconfigs.%s", v1alpha1.GroupVersion.Group)

type provider struct {
	clusterName           string
	datacenterConfig      *v1alpha1.AWSDatacenterConfig
	providerClient        ProviderClient
	providerKubectlClient ProviderKubectlClient
	writer                filewriter.FileWriter
	templateBuilder       *AwsTemplateBuilder
}

type ProviderClient interface {
	BootstrapIam(ctx context.Context, envs map[string]string, fileName string) error
	BootstrapCreds(ctx context.Context, envs map[string]string) (string, error)
	DeleteCloudformationStack(ctx context.Context, envs map[string]string, fileName string) error
}

type ProviderKubectlClient interface {
	GetEksaCluster(ctx context.Context, cluster *types.Cluster) (*v1alpha1.Cluster, error)
	GetEksaAWSDatacenterConfig(ctx context.Context, awsDatacenterConfigName string, kubeconfigFile string) (*v1alpha1.AWSDatacenterConfig, error)
	GetKubeadmControlPlane(ctx context.Context, cluster *types.Cluster, opts ...executables.KubectlOpt) (*kubeadmnv1alpha3.KubeadmControlPlane, error)
	GetMachineDeployment(ctx context.Context, cluster *types.Cluster, opts ...executables.KubectlOpt) (*v1alpha3.MachineDeployment, error)
}

func NewProvider(providerConfig *v1alpha1.AWSDatacenterConfig, clusterName string, providerClient ProviderClient, providerKubectlClient ProviderKubectlClient, writer filewriter.FileWriter, now types.NowFunc) providers.Provider {
	logger.Info("######################### WARNING ##########################")
	logger.Info("############### AWS provider is experimental ###############")
	logger.Info("Make sure your credentials will not expire while you create/delete a cluster.")
	logger.Info("######################### WARNING ##########################")
	return &provider{
		clusterName:           clusterName,
		datacenterConfig:      providerConfig,
		providerClient:        providerClient,
		providerKubectlClient: providerKubectlClient,
		writer:                writer,
		templateBuilder: &AwsTemplateBuilder{
			templateWriter: templater.New(writer),
			awsSpec:        &providerConfig.Spec,
			now:            now,
		},
	}
}

func (p *provider) EnvMap() (map[string]string, error) {
	envMap := make(map[string]string)
	for _, key := range requiredEnvVars {
		if env, ok := os.LookupEnv(key); ok && len(env) > 0 {
			envMap[key] = env
		} else {
			return envMap, fmt.Errorf("warning required env not set %s", key)
		}
	}
	encodedCred := "AWS_B64ENCODED_CREDENTIALS"
	if env, ok := os.LookupEnv(encodedCred); ok && len(env) > 0 {
		envMap[encodedCred] = env
	}
	return envMap, nil
}

func (p *provider) UpdateKubeConfig(content *[]byte, clusterName string) error {
	// customize generated kube config
	return nil
}

func (p *provider) BootstrapClusterOpts() ([]bootstrapper.BootstrapClusterOption, error) {
	env, err := p.EnvMap()
	if err != nil {
		return nil, err
	}

	return []bootstrapper.BootstrapClusterOption{bootstrapper.WithEnv(env)}, nil
}

func (p *provider) Name() string {
	return ProviderName
}

func (p *provider) DatacenterResourceType() string {
	return eksaAwsResourceType
}

func (p *provider) MachineResourceType() string {
	return ""
}

func (p *provider) SetupAndValidateCreateCluster(ctx context.Context, _ *cluster.Spec) error {
	err := p.setupAWSCredentials(ctx)
	if err != nil {
		return fmt.Errorf("error setting up aws creds: %v", err)
	}
	return nil
}

func (p *provider) SetupAndValidateDeleteCluster(ctx context.Context) error {
	err := p.setupAWSCredentials(ctx)
	if err != nil {
		return fmt.Errorf("error setting up aws creds: %v", err)
	}
	return nil
}

func (p *provider) SetupAndValidateUpgradeCluster(ctx context.Context, _ *cluster.Spec) error {
	err := p.setupAWSCredentials(ctx)
	if err != nil {
		return fmt.Errorf("error setting up aws creds: %v", err)
	}
	return nil
}

func (p *provider) UpdateSecrets(ctx context.Context, cluster *types.Cluster) error {
	// Not implemented
	return nil
}

type AwsTemplateBuilder struct {
	awsSpec        *v1alpha1.AWSDatacenterConfigSpec
	templateWriter *templater.Templater
	now            types.NowFunc
}

func (a *AwsTemplateBuilder) WorkerMachineTemplateName(clusterName string) string {
	t := a.now().UnixNano() / int64(time.Millisecond)
	return fmt.Sprintf("%s-worker-node-template-%d", clusterName, t)
}

func (a *AwsTemplateBuilder) CPMachineTemplateName(clusterName string) string {
	t := a.now().UnixNano() / int64(time.Millisecond)
	return fmt.Sprintf("%s-control-plane-template-%d", clusterName, t)
}

func (a *AwsTemplateBuilder) GenerateDeploymentFile(clusterSpec *cluster.Spec, buildOptions ...providers.BuildMapOption) (content []byte, err error) {
	values := BuildTemplateMap(clusterSpec, *a.awsSpec)
	for _, buildOption := range buildOptions {
		buildOption(values)
	}

	bytes, err := templater.Execute(defaultClusterConfig, values)
	if err != nil {
		return nil, err
	}

	return bytes, nil
}

func BuildTemplateMap(clusterSpec *cluster.Spec, awsSpec v1alpha1.AWSDatacenterConfigSpec) map[string]interface{} {
	amiID := awsSpec.AmiID
	if amiID == "" {
		amiID = defaultAmiID
	}

	region := awsSpec.Region
	if region == "" {
		region = defaultAWSRegion
	}

	bundle := clusterSpec.VersionsBundle

	values := map[string]interface{}{
		"clusterName":          clusterSpec.ObjectMeta.Name,
		"kubernetesRepository": bundle.KubeDistro.Kubernetes.Repository,
		"kubernetesVersion":    bundle.KubeDistro.Kubernetes.Tag,
		"etcdRepository":       bundle.KubeDistro.Etcd.Repository,
		"etcdVersion":          bundle.KubeDistro.Etcd.Tag,
		"corednsRepository":    bundle.KubeDistro.CoreDNS.Repository,
		"corednsVersion":       bundle.KubeDistro.CoreDNS.Tag,
		"controlPlaneReplicas": strconv.Itoa(clusterSpec.Spec.ControlPlaneConfiguration.Count),
		"workerNodeReplicas":   strconv.Itoa(clusterSpec.Spec.WorkerNodeGroupConfigurations[0].Count),
		"region":               region,
		"amiID":                amiID,
		"extraArgs":            clusterapi.OIDCToExtraArgs(clusterSpec.OIDCConfig).ToPartialYaml(),
	}
	return values
}

func NeedsNewControlPlaneTemplate(oldC, newC *v1alpha1.Cluster, oldAc, newAc *v1alpha1.AWSDatacenterConfig) bool {
	if oldC.Spec.KubernetesVersion != newC.Spec.KubernetesVersion {
		return true
	}
	if oldAc.Spec.AmiID != newAc.Spec.AmiID {
		return true
	}
	return false
}

func NeedsNewWorkloadTemplate(oldC, newC *v1alpha1.Cluster, oldAc, newAc *v1alpha1.AWSDatacenterConfig) bool {
	if oldC.Spec.KubernetesVersion != newC.Spec.KubernetesVersion {
		return true
	}
	if oldAc.Spec.AmiID != newAc.Spec.AmiID {
		return true
	}
	return false
}

func (p *provider) generateTemplateValuesForUpgrade(ctx context.Context, bootstrapCluster, workloadCluster *types.Cluster, clusterSpec *cluster.Spec) ([]byte, error) {
	clusterName := clusterSpec.ObjectMeta.Name
	var controlPlaneTemplateName string
	var workloadTemplateName string

	c, err := p.providerKubectlClient.GetEksaCluster(ctx, workloadCluster)
	if err != nil {
		return nil, err
	}
	ac, err := p.providerKubectlClient.GetEksaAWSDatacenterConfig(ctx, p.datacenterConfig.Name, workloadCluster.KubeconfigFile)
	if err != nil {
		return nil, err
	}

	needsNewControlPlaneTemplate := NeedsNewControlPlaneTemplate(c, clusterSpec.Cluster, ac, p.datacenterConfig)
	if !needsNewControlPlaneTemplate {
		cp, err := p.providerKubectlClient.GetKubeadmControlPlane(ctx, workloadCluster, executables.WithCluster(bootstrapCluster))
		if err != nil {
			return nil, err
		}
		controlPlaneTemplateName = cp.Spec.InfrastructureTemplate.Name
	} else {
		controlPlaneTemplateName = p.templateBuilder.CPMachineTemplateName(clusterName)
	}

	needsNewWorkloadTemplate := NeedsNewWorkloadTemplate(c, clusterSpec.Cluster, ac, p.datacenterConfig)
	if !needsNewWorkloadTemplate {
		md, err := p.providerKubectlClient.GetMachineDeployment(ctx, workloadCluster, executables.WithCluster(bootstrapCluster))
		if err != nil {
			return nil, err
		}
		workloadTemplateName = md.Spec.Template.Spec.InfrastructureRef.Name
	} else {
		workloadTemplateName = p.templateBuilder.WorkerMachineTemplateName(clusterName)
	}

	valuesOpt := func(values map[string]interface{}) {
		values["needsNewControlPlaneTemplate"] = needsNewControlPlaneTemplate
		values["controlPlaneTemplateName"] = controlPlaneTemplateName
		values["needsNewWorkloadTemplate"] = needsNewWorkloadTemplate
		values["workloadTemplateName"] = workloadTemplateName
	}
	return p.templateBuilder.GenerateDeploymentFile(clusterSpec, valuesOpt)
}

func (p *provider) generateTemplateValuesForCreate(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec) ([]byte, error) {
	clusterName := clusterSpec.ObjectMeta.Name

	valuesOpt := func(values map[string]interface{}) {
		values["needsNewControlPlaneTemplate"] = true
		values["needsNewWorkloadTemplate"] = true
		values["controlPlaneTemplateName"] = p.templateBuilder.CPMachineTemplateName(clusterName)
		values["workloadTemplateName"] = p.templateBuilder.WorkerMachineTemplateName(clusterName)
	}
	return p.templateBuilder.GenerateDeploymentFile(clusterSpec, valuesOpt)
}

func (p *provider) generateDeploymentFile(ctx context.Context, fileName string, content []byte) (string, error) {
	t := templater.New(p.writer)
	writtenFile, err := t.WriteBytesToFile(content, fileName)
	if err != nil {
		return "", fmt.Errorf("error creating cluster config file: %v", err)
	}

	return writtenFile, nil
}

func (p *provider) GenerateDeploymentFileForCreate(ctx context.Context, cluster *types.Cluster, clusterSpec *cluster.Spec, fileName string) (string, error) {
	content, err := p.generateTemplateValuesForCreate(ctx, cluster, clusterSpec)
	if err != nil {
		return "", fmt.Errorf("error generating template values for cluster config file: %v", err)
	}
	return p.generateDeploymentFile(ctx, fileName, content)
}

func (p *provider) GenerateDeploymentFileForUpgrade(ctx context.Context, bootstrapCluster, workloadCluster *types.Cluster, clusterSpec *cluster.Spec, fileName string) (string, error) {
	content, err := p.generateTemplateValuesForUpgrade(ctx, bootstrapCluster, workloadCluster, clusterSpec)
	if err != nil {
		return "", fmt.Errorf("error generating template values for cluster config file: %v", err)
	}
	return p.generateDeploymentFile(ctx, fileName, content)
}

func (p *provider) GenerateStorageClass() []byte {
	return nil
}

func (p *provider) GenerateMHC() ([]byte, error) {
	return []byte{}, nil
}

func (p *provider) CleanupProviderInfrastructure(ctx context.Context) error {
	iamConfigFile, err := p.createIAMConfigFile()
	if err != nil {
		return err
	}

	envMap, err := p.EnvMap()
	if err != nil {
		return err
	}

	return p.providerClient.DeleteCloudformationStack(ctx, envMap, iamConfigFile)
}

func (p *provider) BootstrapSetup(ctx context.Context, clusterConfig *v1alpha1.Cluster, cluster *types.Cluster) error {
	return nil
}

func (p *provider) Version(clusterSpec *cluster.Spec) string {
	return clusterSpec.VersionsBundle.Aws.Version
}

func (p *provider) GetDeployments() map[string][]string {
	return map[string][]string{
		"capa-system":         {"capa-controller-manager"},
		"capi-webhook-system": {"capa-controller-manager"},
	}
}

func (p *provider) GetInfrastructureBundle(clusterSpec *cluster.Spec) *types.InfrastructureBundle {
	bundle := clusterSpec.VersionsBundle
	folderName := fmt.Sprintf("infrastructure-aws/%s/", bundle.Aws.Version)

	infraBundle := types.InfrastructureBundle{
		FolderName: folderName,
		Manifests: []releasev1alpha1.Manifest{
			bundle.Aws.Components,
			bundle.Aws.Metadata,
			bundle.Aws.ClusterTemplate,
		},
	}

	return &infraBundle
}

func (p *provider) DatacenterConfig() providers.DatacenterConfig {
	return p.datacenterConfig
}

func (p *provider) MachineConfigs() []providers.MachineConfig {
	return nil
}

func (p *provider) ValidateNewSpec(_ context.Context, _ *types.Cluster) error {
	return nil
}
