package apps

import (
	"encoding/base64"
	"fmt"

	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/secreturl"

	"io"
	"io/ioutil"
	"strings"

	"gopkg.in/AlecAivazis/survey.v1/terminal"

	"github.com/ghodss/yaml"
	"github.com/jenkins-x/jx/pkg/environments"
	"github.com/jenkins-x/jx/pkg/log"
	"github.com/jenkins-x/jx/pkg/util"

	"github.com/jenkins-x/jx/pkg/surveyutils"
	"github.com/jenkins-x/jx/pkg/vault"

	jenkinsv1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"

	"github.com/jenkins-x/jx/pkg/client/clientset/versioned"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	//ValuesAnnotation is the name of the annotation used to stash values
	ValuesAnnotation       = "jenkins.io/values.yaml"
	appsGeneratedSecretKey = "appsGeneratedSecrets"
)

const secretTemplate = `
{{- range .Values.generatedSecrets }}
apiVersion: v1
data:
  {{ .key }}: {{ .value }}
kind: Secret
metadata:
  name: {{ .name }} 
type: Opaque
{{- end }}
`

// StashValues takes the values used to configure an app and annotates the APP CRD with them allowing them to be used
// at a later date e.g. when the app is upgraded
func StashValues(values []byte, name string, jxClient versioned.Interface, ns string, chartDir string, repository string) (bool, *jenkinsv1.App, error) {
	// locate the app CRD
	create := false
	app, err := jxClient.JenkinsV1().Apps(ns).Get(name, metav1.GetOptions{})
	if err != nil {
		create = true
		app = &jenkinsv1.App{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ns,
			},
			Spec: jenkinsv1.AppSpec{},
		}
	}
	// base64 encode the values.yaml
	encoded := base64.StdEncoding.EncodeToString(values)
	if app.Annotations == nil {
		app.Annotations = make(map[string]string)
	}
	app.Annotations[ValuesAnnotation] = encoded

	err = environments.AddAppMetaData(chartDir, app, repository)
	if err != nil {
		return false, nil, errors.Wrapf(err, "adding metadata to app %s", app.Name)
	}

	return create, app, nil
}

// AddSecretsToVault adds the generatedSecrets into the vault using client at basepath
func AddSecretsToVault(generatedSecrets []*surveyutils.GeneratedSecret, client vault.Client) (func(), error) {
	if len(generatedSecrets) > 0 {
		for _, secret := range generatedSecrets {
			err := vault.WriteMap(client, secret.Path, map[string]interface{}{
				secret.Key: secret.Value,
			})
			if err != nil {
				return func() {}, err
			}
		}
	}
	return func() {}, nil
}

// AddValuesToChart adds a values file to the chart rooted at dir
func AddValuesToChart(name string, values []byte, verbose bool) (string, func(), error) {
	valuesYaml, err := yaml.JSONToYAML(values)
	if err != nil {
		return "", func() {}, errors.Wrapf(err, "error converting values from json to yaml\n\n%v", values)
	}
	if verbose {
		log.Logger().Infof("Generated values.yaml:\n\n%v", util.ColorInfo(string(valuesYaml)))
	}

	valuesFile, err := ioutil.TempFile("", fmt.Sprintf("%s-values.yaml", ToValidFileSystemName(name)))
	cleanup := func() {
		err = valuesFile.Close()
		if err != nil {
			log.Logger().Warnf("Error closing %s because %v", valuesFile.Name(), err)
		}
		err = util.DeleteFile(valuesFile.Name())
		if err != nil {
			log.Logger().Warnf("Error deleting %s because %v", valuesFile.Name(), err)
		}
	}
	if err != nil {
		return "", func() {}, errors.Wrapf(err, "creating tempfile to write values for %s", name)
	}
	_, err = valuesFile.Write(valuesYaml)
	if err != nil {
		return "", func() {}, errors.Wrapf(err, "writing values to %s for %s", valuesFile.Name(), name)
	}
	return valuesFile.Name(), cleanup, nil
}

//GenerateQuestions asks questions based on the schema
func GenerateQuestions(schema []byte, batchMode bool, askExisting bool, basePath string, useVault bool,
	existing map[string]interface{}, secretsScheme string, in terminal.FileReader, out terminal.FileWriter, outErr io.Writer) ([]byte,
	[]*surveyutils.GeneratedSecret, error) {
	secrets := make([]*surveyutils.GeneratedSecret, 0)
	schemaOptions := surveyutils.JSONSchemaOptions{
		CreateSecret: func(name string, key string, value string) (interface{},
			error) {
			secret := &surveyutils.GeneratedSecret{
				Name:  name,
				Key:   key,
				Value: value,
				Path:  strings.Join([]string{basePath, name}, "/"),
			}
			secrets = append(secrets, secret)
			if useVault {
				return secreturl.ToURI(secret.Path, secret.Key, secretsScheme), nil
			}
			return value, nil
		},
		Out:                 out,
		In:                  in,
		OutErr:              outErr,
		IgnoreMissingValues: false,
		NoAsk:               batchMode,
		AutoAcceptDefaults:  batchMode,
		AskExisting:         askExisting,
	}
	// For adding an app there are by definition no existing values,
	// and whether we auto-accept defaults is determined by batch mode
	values, err := schemaOptions.GenerateValues(schema, existing)
	if err != nil {
		return nil, nil, errors.WithStack(err)
	}
	return values, secrets, nil
}

func addApp(create bool, jxClient versioned.Interface, app *jenkinsv1.App) error {
	if create {
		_, err := jxClient.JenkinsV1().Apps(app.Namespace).Create(app)
		if err != nil {
			return errors.Wrapf(err, "creating App %s to annotate with values.yaml", app.Name)
		}
	} else {
		_, err := jxClient.JenkinsV1().Apps(app.Namespace).PatchUpdate(app)
		if err != nil {
			return errors.Wrapf(err, "updating App %s to annotate with values.yaml", app.Name)
		}
	}
	return nil
}

// ProcessValues is responsible for taking a schema, asking questions of the user (using in, out and outErr), and generating
// a yaml file that contains the answers. The path to the yaml file is returned, along with a function to cleanup temporary
// resources, including the yaml file. The gitOpsURL, if specified, is used to determine the path to store the secrets in the
// vault, otherwise the team name is used. If batchMode is true, it alters the way questions are asked, trying to use existing answers or defaults
// where possible. If askExisting is true then all questions, even those with existing answers are asked. The vault client is
// used to store secrets, and the secretsScheme is used as the scheme part of the url to the secret.
func ProcessValues(
	schema []byte,
	name string,
	gitOpsURL string,
	teamName string,
	batchMode bool,
	askExisting bool,
	vaultClient vault.Client,
	existing map[string]interface{},
	secretsScheme string,
	in terminal.FileReader,
	out terminal.FileWriter,
	outErr io.Writer,
	verbose bool) (string, func(), error) {
	var values []byte
	var generatedSecrets []*surveyutils.GeneratedSecret
	var basepath string
	var err error
	if gitOpsURL != "" {
		gitInfo, err := gits.ParseGitURL(gitOpsURL)
		if err != nil {
			return "", func() {}, err
		}
		basepath = strings.Join([]string{"gitOps", gitInfo.Organisation, gitInfo.Name}, "/")
	} else {
		basepath = strings.Join([]string{"teams", teamName}, "/")
	}
	values, generatedSecrets, err = GenerateQuestions(schema, batchMode, askExisting, basepath, vaultClient != nil, existing, secretsScheme, in, out, outErr)
	if err != nil {
		return "", func() {}, errors.Wrapf(err, "asking questions for schema")
	}
	valuesFileName, cleanupValues, err := AddValuesToChart(name, values, verbose)
	cleanup := func() {
		cleanupValues()
	}
	if err != nil {
		return "", cleanup, err
	}
	if vaultClient != nil {
		cleanupSecrets, err := AddSecretsToVault(generatedSecrets, vaultClient)
		cleanup = func() {
			cleanupValues()
			cleanupSecrets()
		}
		if err != nil {
			return "", cleanup, errors.Wrapf(err, "adding generatedSecrets to vault for %s", name)
		}
	} else {
		// TODO Add support local vault
	}
	return valuesFileName, cleanup, nil
}
