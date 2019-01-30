package cmd

import (
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/jenkins-x/jx/pkg/gits"
	"github.com/jenkins-x/jx/pkg/surveyutils"
	"github.com/jenkins-x/jx/pkg/vault"

	"github.com/ghodss/yaml"
	"github.com/jenkins-x/jx/pkg/util"
	"github.com/pkg/errors"

	jenkinsv1 "github.com/jenkins-x/jx/pkg/apis/jenkins.io/v1"
	"github.com/jenkins-x/jx/pkg/extensions"
	"github.com/jenkins-x/jx/pkg/kube"
	"github.com/jenkins-x/jx/pkg/log"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// GetDevEnv gets the Development Enviornment CRD as devEnv,
// and also tells the user whether the development environment is using gitOps
func (o *CommonOptions) GetDevEnv() (gitOps bool, devEnv *jenkinsv1.Environment) {

	// We're going to need to know whether the team is using GitOps for the dev env or not,
	// and also access the team settings, so load those
	jxClient, ns, err := o.JXClientAndDevNamespace()
	if err != nil {
		if o.Verbose {
			log.Errorf("Error loading team settings. %v\n", err)
		}
		return false, &jenkinsv1.Environment{}
	} else {
		devEnv, err := kube.GetDevEnvironment(jxClient, ns)
		if err != nil {
			log.Errorf("Error loading team settings. %v\n", err)
			return false, &jenkinsv1.Environment{}
		}
		gitOps := false
		if devEnv.Spec.Source.URL != "" {
			gitOps = true
		}
		return gitOps, devEnv
	}
}

// OnAppInstall calls extensions.OnAppInstall for the current cmd, passing app and version
func (o *CommonOptions) OnAppInstall(app string, version string) error {
	// Find the app metadata, if any
	jxClient, ns, err := o.JXClientAndDevNamespace()
	if err != nil {
		return err
	}
	kubeClient, _, err := o.KubeClientAndDevNamespace()
	if err != nil {
		return err
	}
	certClient, err := o.CreateCertManagerClient()
	if err != nil {
		return err
	}
	selector := fmt.Sprintf("chart=%s-%s", app, version)
	appList, err := jxClient.JenkinsV1().Apps(ns).List(metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return err
	}
	if len(appList.Items) > 1 {
		return fmt.Errorf("more than one app (%v) was found for %s", appList.Items, selector)
	} else if len(appList.Items) == 1 {
		return extensions.OnInstallFromName(app, jxClient, kubeClient, certClient, ns, o.Helm(), defaultInstallTimeout)
	}
	return nil
}

func (o *AddAppOptions) handleValues(dir string, app string, values []byte) (func(), error) {
	valuesYaml, err := yaml.JSONToYAML(values)
	if err != nil {
		return func() {}, errors.Wrapf(err, "error converting values from json to yaml\n\n%v", values)
	}
	if o.Verbose {
		log.Infof("Generated values.yaml:\n\n%v\n", util.ColorInfo(string(valuesYaml)))
	}

	valuesFile, err := ioutil.TempFile("", fmt.Sprintf("%s-values.yaml", app))
	close := func() {
		err = valuesFile.Close()
		if err != nil {
			log.Warnf("Error closing %s because %v\n", valuesFile.Name(), err)
		}
		err = util.DeleteFile(valuesFile.Name())
		if err != nil {
			log.Warnf("Error deleting %s because %v\n", valuesFile.Name(), err)
		}
	}
	if err != nil {
		return close, err
	}
	_, err = valuesFile.Write(valuesYaml)
	if err != nil {
		return close, err
	}

	if o.ValuesFiles == nil {
		o.ValuesFiles = make([]string, 0)
	}
	o.ValuesFiles = append(o.ValuesFiles, valuesFile.Name())
	return close, nil
}

func (o *AddAppOptions) stashValues(values []byte, name string) error {
	jxClient, ns, err := o.JXClientAndDevNamespace()
	if err != nil {
		return err
	}

}

func (o *AddAppOptions) initVault() (vault.Client, string, error) {
	var vaultBasepath string
	var vaultClient vault.Client

	var err error
	if o.GitOps {
		gitInfo, err := gits.ParseGitURL(o.DevEnv.Spec.Source.URL)
		if err != nil {
			return nil, "", err
		}
		vaultBasepath = strings.Join([]string{"gitOps", gitInfo.Organisation, gitInfo.Name}, "/")
	} else {

		teamName, _, err := o.TeamAndEnvironmentNames()
		if err != nil {
			return nil, "", err
		}
		vaultBasepath = strings.Join([]string{"teams", teamName}, "/")
	}
	vaultClient, err = o.CreateSystemVaultClient("")
	if err != nil {
		return nil, "", err
	}
	return vaultClient, vaultBasepath, nil
}

func (o *AddAppOptions) handleSecrets(dir string, app string, secrets []*surveyutils.GeneratedSecret) (func(), error) {

	// We write a secret template into the chart, append the values for the generated secrets to values.yaml
	if len(secrets) > 0 {

		if o.UseVault() {
			vaultClient, vaultBasepath, err := o.initVault()
			if err != nil {
				return func() {}, err
			}
			for _, secret := range secrets {
				path := strings.Join([]string{vaultBasepath, secret.Name}, "/")
				err := vault.WriteMap(vaultClient, path, map[string]interface{}{
					secret.Key: secret.Value,
				})
				if err != nil {
					return func() {}, err
				}
			}
		} else {
			// For each secret, we write a file into the chart
			templatesDir := filepath.Join(dir, "templates")
			err := os.MkdirAll(templatesDir, 0700)
			if err != nil {
				return func() {}, err
			}
			fileName := filepath.Join(templatesDir, "app-generated-secret-template.yaml")
			err = ioutil.WriteFile(fileName, []byte(secretTemplate), 0755)
			if err != nil {
				return func() {}, err
			}
			allSecrets := map[string][]*surveyutils.GeneratedSecret{
				appsGeneratedSecretKey: secrets,
			}
			secretsYaml, err := yaml.Marshal(allSecrets)
			if err != nil {
				return func() {}, err
			}
			secretsFile, err := ioutil.TempFile("", fmt.Sprintf("%s-secrets.yaml", app))
			close := func() {
				err = secretsFile.Close()
				if err != nil {
					log.Warnf("Error closing %s because %v\n", secretsFile.Name(), err)
				}
				err = util.DeleteFile(secretsFile.Name())
				if err != nil {
					log.Warnf("Error deleting %s because %v\n", secretsFile.Name(), err)
				}
			}
			if err != nil {
				return close, err
			}
			_, err = secretsFile.Write(secretsYaml)
			if err != nil {
				return close, err
			}
			if o.ValuesFiles == nil {
				o.ValuesFiles = make([]string, 0)
			}
			o.ValuesFiles = append(o.ValuesFiles, secretsFile.Name())
		}
	}
	return func() {}, nil
}

func (o *AddAppOptions) generateQuestions(schema []byte) ([]byte, []*surveyutils.GeneratedSecret, error) {
	secrets := make([]*surveyutils.GeneratedSecret, 0)

	schemaOptions := surveyutils.JSONSchemaOptions{
		CreateSecret: func(name string, key string, value string) (*jenkinsv1.ResourceReference, error) {
			secret := &surveyutils.GeneratedSecret{
				Name:  name,
				Key:   key,
				Value: value,
			}
			secrets = append(secrets, secret)
			return &jenkinsv1.ResourceReference{
				Name: name,
				Kind: "Secret",
			}, nil

		},
	}

	values, err := schemaOptions.GenerateValues(schema, []string{}, o.In, o.Out, o.Err)
	if err != nil {
		return nil, nil, err
	}
	return values, secrets, nil
}
