package get

import (
	"fmt"
	"os"
	"strings"
	"time"

	v1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/pkg/builds"
	"github.com/jenkins-x/jx/pkg/client/clientset/versioned"
	"github.com/jenkins-x/jx/pkg/cmd/helper"
	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/kube"
	"github.com/jenkins-x/jx/pkg/logs"
	"github.com/jenkins-x/jx/pkg/tekton"
	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline"
	"k8s.io/client-go/kubernetes"

	"github.com/jenkins-x/jx/pkg/cmd/opts"
	"github.com/jenkins-x/jx/pkg/cmd/templates"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"
	tektonclient "github.com/tektoncd/pipeline/pkg/client/clientset/versioned"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetBuildLogsOptions the command line options
type GetBuildLogsOptions struct {
	GetOptions

	Tail                    bool
	Wait                    bool
	BuildFilter             builds.BuildPodInfoFilter
	CurrentFolder           bool
	WaitForPipelineDuration time.Duration
	TektonLogger            *logs.TektonLogger
	FailIfPodFails          bool
}

// CLILogWriter is an implementation of logs.LogWriter that will show logs in the standard output
type CLILogWriter struct {
	*opts.CommonOptions
}

var (
	get_build_log_long = templates.LongDesc(`
		Display a build log

`)

	get_build_log_example = templates.Examples(`
		# Display a build log - with the user choosing which repo + build to view
		jx get build log

		# Pick a build to view the log based on the repo cheese
		jx get build log --repo cheese

		# Pick a pending Tekton build to view the log based 
		jx get build log -p

		# Pick a pending Tekton build to view the log based on the repo cheese
		jx get build log --repo cheese -p

		# Pick a Tekton build for the 1234 Pull Request on the repo cheese
		jx get build log --repo cheese --branch PR-1234

		# View the build logs for a specific tekton build pod
		jx get build log --pod my-pod-name
	`)
)

// NewCmdGetBuildLogs creates the command
func NewCmdGetBuildLogs(commonOpts *opts.CommonOptions) *cobra.Command {
	options := &GetBuildLogsOptions{
		GetOptions: GetOptions{
			CommonOptions: commonOpts,
		},
	}

	cmd := &cobra.Command{
		Use:     "log [flags]",
		Short:   "Display a build log",
		Long:    get_build_log_long,
		Example: get_build_log_example,
		Aliases: []string{"logs"},
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().BoolVarP(&options.Tail, "tail", "t", true, "Tails the build log to the current terminal")
	cmd.Flags().BoolVarP(&options.Wait, "wait", "w", false, "Waits for the build to start before failing")
	cmd.Flags().BoolVarP(&options.FailIfPodFails, "fail-with-pod", "", false, "Return an error if the pod fails")
	cmd.Flags().DurationVarP(&options.WaitForPipelineDuration, "wait-duration", "d", time.Minute*5, "Timeout period waiting for the given pipeline to be created")
	cmd.Flags().BoolVarP(&options.BuildFilter.Pending, "pending", "p", false, "Only display logs which are currently pending to choose from if no build name is supplied")
	cmd.Flags().StringVarP(&options.BuildFilter.Filter, "filter", "f", "", "Filters all the available jobs by those that contain the given text")
	cmd.Flags().StringVarP(&options.BuildFilter.Owner, "owner", "o", "", "Filters the owner (person/organisation) of the repository")
	cmd.Flags().StringVarP(&options.BuildFilter.Repository, "repo", "r", "", "Filters the build repository")
	cmd.Flags().StringVarP(&options.BuildFilter.Branch, "branch", "", "", "Filters the branch")
	cmd.Flags().StringVarP(&options.BuildFilter.Build, "build", "", "", "The build number to view")
	cmd.Flags().StringVarP(&options.BuildFilter.Pod, "pod", "", "", "The pod name to view")
	cmd.Flags().StringVarP(&options.BuildFilter.GitURL, "giturl", "g", "", "The git URL to filter on. If you specify a link to a github repository or PR we can filter the query of build pods accordingly")
	cmd.Flags().StringVarP(&options.BuildFilter.Context, "context", "", "", "Filters the context of the build")
	cmd.Flags().BoolVarP(&options.CurrentFolder, "current", "c", false, "Display logs using current folder as repo name, and parent folder as owner")
	options.AddBaseFlags(cmd)

	return cmd
}

// Run implements this command
func (o *GetBuildLogsOptions) Run() error {
	err := o.BuildFilter.Validate()
	if err != nil {
		return err
	}
	jxClient, ns, err := o.JXClientAndDevNamespace()
	if err != nil {
		return err
	}
	kubeClient, err := o.KubeClient()
	if err != nil {
		return err
	}
	tektonClient, _, err := o.TektonClient()
	if err != nil {
		return err
	}

	tektonEnabled, err := kube.IsTektonEnabled(kubeClient, ns)
	if err != nil {
		return err
	}

	return o.getProwBuildLog(kubeClient, tektonClient, jxClient, ns, tektonEnabled)
}

// getProwBuildLog prompts the user, if needed, to choose a pipeline, and then prints out that pipeline's logs.
func (o *GetBuildLogsOptions) getProwBuildLog(kubeClient kubernetes.Interface, tektonClient tektonclient.Interface, jxClient versioned.Interface, ns string, tektonEnabled bool) error {
	if o.CurrentFolder {
		currentDirectory, err := os.Getwd()
		if err != nil {
			return err
		}

		gitRepository, err := gits.NewGitCLI().Info(currentDirectory)
		if err != nil {
			return err
		}

		o.BuildFilter.Repository = gitRepository.Name
		o.BuildFilter.Owner = gitRepository.Organisation
	}

	var err error

	if o.TektonLogger == nil {
		o.TektonLogger = &logs.TektonLogger{
			KubeClient:   kubeClient,
			TektonClient: tektonClient,
			JXClient:     jxClient,
			Namespace:    ns,
			LogWriter: &CLILogWriter{
				CommonOptions: o.CommonOptions,
			},
			FailIfPodFails: o.FailIfPodFails,
		}
	}
	var waitableCondition bool
	f := func() error {
		waitableCondition, err = o.getTektonLogs(kubeClient, tektonClient, jxClient, ns)
		return err
	}

	err = f()
	if err != nil {
		if o.Wait && waitableCondition {
			log.Logger().Info("The selected pipeline didn't start, let's wait a bit")
			err := util.Retry(o.WaitForPipelineDuration, f)
			if err != nil {
				return err
			}
		}
		return err
	}
	return nil
}

func (o *GetBuildLogsOptions) getTektonLogs(kubeClient kubernetes.Interface, tektonClient tektonclient.Interface, jxClient versioned.Interface, ns string) (bool, error) {
	var defaultName string

	names, paMap, err := o.TektonLogger.GetTektonPipelinesWithActivePipelineActivity(o.BuildFilter.LabelSelectorsForActivity())
	if err != nil {
		return true, err
	}

	var filter string
	if len(o.Args) > 0 {
		filter = o.Args[0]
	} else {
		filter = o.BuildFilter.Filter
	}

	var filteredNames []string
	for _, n := range names {
		if strings.Contains(strings.ToLower(n), strings.ToLower(filter)) {
			filteredNames = append(filteredNames, n)
		}
	}

	if o.BatchMode {
		if len(filteredNames) > 1 {
			return false, errors.New("more than one pipeline returned in batch mode, use better filters and try again")
		}
		if len(filteredNames) == 1 {
			defaultName = filteredNames[0]
		}
	}

	name, err := util.PickNameWithDefault(filteredNames, "Which build do you want to view the logs of?: ", defaultName, "", o.GetIOFileHandles())
	if err != nil {
		return len(filteredNames) == 0, err
	}

	pa, exists := paMap[name]
	if !exists {
		return true, errors.New("there are no build logs for the supplied filters")
	}

	if pa.Spec.BuildLogsURL != "" {
		authSvc, err := o.GitAuthConfigService()
		if err != nil {
			return false, err
		}
		return false, o.TektonLogger.StreamPipelinePersistentLogs(pa.Spec.BuildLogsURL, jxClient, ns, authSvc)
	}

	log.Logger().Infof("Build logs for %s", util.ColorInfo(name))
	name = strings.TrimSuffix(name, " ")
	return false, o.TektonLogger.GetRunningBuildLogs(pa, name, false)
}

// StreamLog implementation of LogWriter.StreamLog for CLILogWriter, this implementation will tail logs for the provided pod /container through the defined logger
func (o *CLILogWriter) StreamLog(lch <-chan logs.LogLine, ech <-chan error) error {
	for {
		select {
		case l, ok := <-lch:
			if !ok {
				return nil
			}
			fmt.Println(l.Line)
		case err := <-ech:
			return err
		}
	}
}

// WriteLog implementation of LogWriter.WriteLog for CLILogWriter, this implementation will write the provided log line through the defined logger
func (o *CLILogWriter) WriteLog(logLine logs.LogLine, lch chan<- logs.LogLine) error {
	lch <- logLine
	return nil
}

// BytesLimit defines the limit of bytes to be used to fetch the logs from the kube API
// defaulted to 0 for this implementation
func (o *CLILogWriter) BytesLimit() int {
	//We are not limiting bytes with this writer
	return 0
}

// loadPipelines loads all available pipelines as PipelineRunInfos.
func (o *GetBuildLogsOptions) loadPipelines(kubeClient kubernetes.Interface, tektonClient tektonclient.Interface, jxClient versioned.Interface, ns string) ([]string, string, map[string][]builds.BaseBuildInfo, error) {
	defaultName := ""
	names := []string{}
	pipelineMap := map[string][]builds.BaseBuildInfo{}

	labelSelectors := o.BuildFilter.LabelSelectorsForBuild()

	listOptions := metav1.ListOptions{}
	if len(labelSelectors) > 0 {
		listOptions.LabelSelector = strings.Join(labelSelectors, ",")
	}

	prList, err := tektonClient.TektonV1alpha1().PipelineRuns(ns).List(listOptions)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Logger().Warnf("Failed to query PipelineRuns %s", err)
		return names, defaultName, pipelineMap, err
	}

	structures, err := jxClient.JenkinsV1().PipelineStructures(ns).List(listOptions)
	if err != nil && !apierrors.IsNotFound(err) {
		log.Logger().Warnf("Failed to query PipelineStructures %s", err)
		return names, defaultName, pipelineMap, err
	}
	// TODO: Remove this eventually - it's only here for structures created before we started applying labels to them in v2.0.216.
	if len(prList.Items) > len(structures.Items) && len(labelSelectors) != 0 {
		structures, err = jxClient.JenkinsV1().PipelineStructures(ns).List(metav1.ListOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			log.Logger().Warnf("Failed to query PipelineStructures %s", err)
			return names, defaultName, pipelineMap, err
		}
	}

	buildInfos := []*tekton.PipelineRunInfo{}

	podLabelSelector := pipeline.GroupName + pipeline.PipelineRunLabelKey
	if len(labelSelectors) > 0 {
		podLabelSelector += "," + strings.Join(labelSelectors, ",")
	}
	podList, err := kubeClient.CoreV1().Pods(ns).List(metav1.ListOptions{
		LabelSelector: podLabelSelector,
	})
	if err != nil {
		return names, defaultName, pipelineMap, err
	}
	for _, pr := range prList.Items {
		var ps v1.PipelineStructure
		for _, p := range structures.Items {
			if p.Name == pr.Name {
				ps = p
			}
		}
		pri, err := tekton.CreatePipelineRunInfo(pr.Name, podList, &ps, &pr)
		if err != nil {
			log.Logger().Warnf("Error creating PipelineRunInfo for PipelineRun %s: %s", pr.Name, err)
		}
		if pri != nil && o.BuildFilter.BuildMatches(pri.ToBuildPodInfo()) {
			buildInfos = append(buildInfos, pri)
		}
	}

	tekton.SortPipelineRunInfos(buildInfos)
	if len(buildInfos) == 0 {
		return names, defaultName, pipelineMap, fmt.Errorf("no Tekton pipelines have been triggered which match the current filter")
	}

	namesMap := make(map[string]bool, 0)
	for _, build := range buildInfos {
		buildName := build.Pipeline + " #" + build.Build
		if build.Context != "" {
			buildName += " " + build.Context
		}
		namesMap[buildName] = true
		pipelineMap[buildName] = append(pipelineMap[buildName], build)

		if build.Branch == "master" {
			defaultName = buildName
		}
	}
	for k := range namesMap {
		names = append(names, k)
	}

	return names, defaultName, pipelineMap, nil
}

func (o *GetBuildLogsOptions) loadPipelineActivities(jxClient versioned.Interface, ns string) (*v1.PipelineActivityList, error) {
	paList, err := jxClient.JenkinsV1().PipelineActivities(ns).List(metav1.ListOptions{})
	if err != nil {
		return nil, errors.Wrap(err, "there was a problem getting the PipelineActivities")
	}

	return paList, nil
}
