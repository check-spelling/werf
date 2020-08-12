package get_autogenerated_values

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/werf/logboek"

	"github.com/werf/werf/cmd/werf/common"
	helm_common "github.com/werf/werf/cmd/werf/helm/common"
	"github.com/werf/werf/pkg/deploy"
	"github.com/werf/werf/pkg/docker"
	"github.com/werf/werf/pkg/image"
	"github.com/werf/werf/pkg/images_manager"
	"github.com/werf/werf/pkg/ssh_agent"
	"github.com/werf/werf/pkg/true_git"
	"github.com/werf/werf/pkg/util"
	"github.com/werf/werf/pkg/werf"
)

var commonCmdData common.CmdData

func NewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get-autogenerated-values",
		Short: "Get service values yaml generated by werf for helm chart during deploy",
		Long: common.GetLongCommandDescription(`Get service values generated by werf for helm chart during deploy.

These values includes project name, docker images ids and other`),
		DisableFlagsInUseLine: true,
		Annotations: map[string]string{
			common.CmdEnvAnno: common.EnvsDescription(common.WerfSecretKey),
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := common.ProcessLogOptions(&commonCmdData); err != nil {
				common.PrintHelp(cmd)
				return err
			}

			return runGetServiceValues()
		},
	}

	common.SetupDir(&commonCmdData, cmd)
	common.SetupConfigPath(&commonCmdData, cmd)
	common.SetupConfigTemplatesDir(&commonCmdData, cmd)
	common.SetupTmpDir(&commonCmdData, cmd)
	common.SetupHomeDir(&commonCmdData, cmd)
	common.SetupSSHKey(&commonCmdData, cmd)

	common.SetupTag(&commonCmdData, cmd)
	common.SetupEnvironment(&commonCmdData, cmd)
	common.SetupNamespace(&commonCmdData, cmd)

	common.SetupImagesRepoOptions(&commonCmdData, cmd)

	common.SetupDockerConfig(&commonCmdData, cmd, "Command needs granted permissions to read and pull images from the specified stages storage and images repo")
	common.SetupInsecureRegistry(&commonCmdData, cmd)
	common.SetupSkipTlsVerifyRegistry(&commonCmdData, cmd)

	common.SetupLogOptions(&commonCmdData, cmd)

	return cmd
}

func runGetServiceValues() error {
	logboek.Streams().Mute()
	defer logboek.Streams().Unmute()

	ctx := common.BackgroundContext()

	if err := werf.Init(*commonCmdData.TmpDir, *commonCmdData.HomeDir); err != nil {
		return fmt.Errorf("initialization error: %s", err)
	}

	if err := image.Init(); err != nil {
		return err
	}

	if err := deploy.Init(ctx, deploy.InitOptions{WithoutHelm: true}); err != nil {
		return err
	}

	if err := true_git.Init(true_git.Options{Out: logboek.ProxyOutStream(), Err: logboek.ProxyErrStream(), LiveGitOutput: *commonCmdData.LogVerbose || *commonCmdData.LogDebug}); err != nil {
		return err
	}

	if err := common.DockerRegistryInit(&commonCmdData); err != nil {
		return err
	}

	if err := docker.Init(ctx, *commonCmdData.DockerConfig, *commonCmdData.LogVerbose, *commonCmdData.LogDebug); err != nil {
		return err
	}

	projectDir, err := common.GetProjectDir(&commonCmdData)
	if err != nil {
		return fmt.Errorf("getting project dir failed: %s", err)
	}

	werfConfig, err := common.GetRequiredWerfConfig(projectDir, &commonCmdData, false)
	if err != nil {
		return fmt.Errorf("unable to load werf config: %s", err)
	}

	projectName := werfConfig.Meta.Project

	imagesRepoAddress, err := common.GetOptionalImagesRepoAddress(projectName, &commonCmdData)
	if err != nil {
		return err
	}

	withoutRepo := true
	if imagesRepoAddress != "" {
		withoutRepo = false
	}

	imagesRepo, err := common.GetImagesRepoWithOptionalStubRepoAddress(projectName, &commonCmdData)
	if err != nil {
		return err
	}

	environment := helm_common.GetEnvironmentOrStub(*commonCmdData.Environment)

	namespace, err := common.GetKubernetesNamespace(*commonCmdData.Namespace, environment, werfConfig)
	if err != nil {
		return err
	}

	tag, tagStrategy, err := helm_common.GetTagOrStub(&commonCmdData)
	if err != nil {
		return err
	}

	if err := ssh_agent.Init(ctx, *commonCmdData.SSHKeys); err != nil {
		return fmt.Errorf("cannot initialize ssh agent: %s", err)
	}
	defer func() {
		err := ssh_agent.Terminate()
		if err != nil {
			logboek.Warn().LogF("WARNING: ssh agent termination failed: %s\n", err)
		}
	}()

	var imagesInfoGetters []images_manager.ImageInfoGetter
	var imagesNames []string
	for _, imageConfig := range werfConfig.StapelImages {
		imagesNames = append(imagesNames, imageConfig.Name)
	}
	for _, imageConfig := range werfConfig.ImagesFromDockerfile {
		imagesNames = append(imagesNames, imageConfig.Name)
	}
	for _, imageName := range imagesNames {
		d := &images_manager.ImageInfo{
			ImagesRepo:      imagesRepo,
			Name:            imageName,
			Tag:             tag,
			WithoutRegistry: withoutRepo,
		}
		imagesInfoGetters = append(imagesInfoGetters, d)
	}

	serviceValues, err := deploy.GetServiceValues(ctx, projectName, imagesRepo.String(), namespace, tag, tagStrategy, imagesInfoGetters, deploy.ServiceValuesOptions{Env: environment})
	if err != nil {
		return fmt.Errorf("error creating service values: %s", err)
	}

	fmt.Printf("%s", util.DumpYaml(serviceValues))

	return nil
}
