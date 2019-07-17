package pr

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/jenkins-x/jx/pkg/gits"

	"github.com/jenkins-x/jx/pkg/cmd/helper"
	"github.com/jenkins-x/jx/pkg/util"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"

	"github.com/jenkins-x/jx/pkg/cmd/opts"
	"github.com/jenkins-x/jx/pkg/cmd/templates"
	"github.com/jenkins-x/jx/pkg/log"
)

// DefaultPrefix for all PR labels environment keys
const DefaultPrefix = "JX_PR_LABELS"

// StepPRLabelsOptions holds the options for the cmd
type StepPRLabelsOptions struct {
	*opts.CommonOptions

	Dir         string
	Prefix      string
	PullRequest string
	GitURL      string
}

var (
	labelLong = templates.LongDesc(`
		Prints out environment variables from the labels in a pull request.

		The pull request number is set using --pr or read from the BRANCH_NAME environment variable (removing the PR-
		prefix.

		Environment variables are prefixed per default with ` + DefaultPrefix + `.
        You can use the '--prefix' argument to set a different prefix.
    `)

	labelExample = templates.Examples(`
		# List all labels using the environment variable BRANCH_NAME to determine the PR number 
		jx step pr labels

		# List all labels using the environment variable BRANCH_NAME to determine the PR number, running in batch mode 
		jx step pr labels -b 

		# List all labels using a custom prefix
		jx step pr labels -b --prefix PRL

		# List all labels specifying the pull request number
		jx step pr labels -b --pr PR-34
		jx step pr labels -b --pr 34

    `)
)

// NewCmdStepPRLabels creates the new cmd
func NewCmdStepPRLabels(commonOpts *opts.CommonOptions) *cobra.Command {
	options := &StepPRLabelsOptions{
		CommonOptions: commonOpts,
	}
	cmd := &cobra.Command{
		Use:     "labels",
		Short:   "List all labels of a given pull-request",
		Long:    labelLong,
		Example: labelExample,
		Run: func(cmd *cobra.Command, args []string) {
			options.Cmd = cmd
			options.Args = args
			err := options.Run()
			helper.CheckErr(err)
		},
	}
	cmd.Flags().StringVarP(&options.PullRequest, "pr", "", "", "Git Pull Request number")
	cmd.Flags().StringVarP(&options.Prefix, "prefix", "p", "", "Environment variable prefix")
	cmd.Flags().BoolVarP(&options.BatchMode, opts.OptionBatchMode, "b", false, "Enable batch mode")
	cmd.Flags().StringVarP(&options.GitURL, "url", "", "", "Specify the url of the git repo, if not set will read the info from the current directory")
	return cmd
}

// Run implements the execution
func (o *StepPRLabelsOptions) Run() error {

	gitInfo, provider, _, err := o.CreateGitProvider(o.Dir)
	if err != nil {
		return err
	}
	if provider == nil {
		return fmt.Errorf("No Git provider could be found. Are you in a directory containing a `.git/config` file?")
	}

	if o.PullRequest == "" {
		o.PullRequest = strings.TrimPrefix(os.Getenv("BRANCH_NAME"), "PR-")
	}

	if o.PullRequest == "" {
		return util.MissingOption("pr")
	}

	if o.Prefix == "" {
		o.Prefix = DefaultPrefix
	}

	prNum, err := strconv.Atoi(o.PullRequest)
	if err != nil {
		log.Logger().Warn("Unable to convert PR " + o.PullRequest + " to a number")
	}

	if o.GitURL != "" {
		gitInfo, err = gits.ParseGitURL(o.GitURL)
		if err != nil {
			return errors.Wrapf(err, "parsing %s", o.GitURL)
		}
	}

	pr, err := provider.GetPullRequest(gitInfo.Organisation, gitInfo, prNum)
	if err != nil {
		return errors.Wrapf(err, "failed to find PullRequest %d", prNum)
	}

	reg, err := regexp.Compile("[^a-zA-Z0-9]+")
	if err != nil {
		return errors.Wrapf(err, "failed to create regex %v", reg)
	}

	for _, v := range pr.Labels {
		envKey := reg.ReplaceAllString(*v.Name, "_")
		// Must be fmt.Printf as needs to go stdout
		fmt.Printf("%v_%v='%v'", o.Prefix, strings.ToUpper(envKey), *v.Name)
	}
	return nil
}
