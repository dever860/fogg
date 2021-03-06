package apply

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/chanzuckerberg/fogg/config"
	"github.com/chanzuckerberg/fogg/plan"
	"github.com/chanzuckerberg/fogg/plugins"
	"github.com/chanzuckerberg/fogg/templates"
	"github.com/chanzuckerberg/fogg/util"
	"github.com/gobuffalo/packr"
	getter "github.com/hashicorp/go-getter"
	"github.com/hashicorp/hcl/hcl/printer"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/afero"
)

const rootPath = "terraform"

func Apply(fs afero.Fs, conf *config.Config, tmp *templates.T) error {
	p, err := plan.Eval(conf, false)
	if err != nil {
		return errors.Wrap(err, "unable to evaluate plan")
	}

	e := applyRepo(fs, p, &tmp.Repo)
	if e != nil {
		return errors.Wrap(e, "unable to apply repo")
	}

	e = applyAccounts(fs, p, &tmp.Account)
	if e != nil {
		return errors.Wrap(e, "unable to apply accounts")
	}

	e = applyEnvs(fs, p, &tmp.Env, &tmp.Component)
	if e != nil {
		return errors.Wrap(e, "unable to apply envs")
	}

	e = applyGlobal(fs, p.Global, &tmp.Global)
	if e != nil {
		return errors.Wrap(e, "unable to apply global")
	}

	e = applyModules(fs, p.Modules, &tmp.Module)
	return errors.Wrap(e, "unable to apply modules")
}

func applyRepo(fs afero.Fs, p *plan.Plan, repoTemplates *packr.Box) error {
	e := applyTree(fs, repoTemplates, "", p)
	if e != nil {
		return e
	}
	return applyPlugins(fs, p)
}

func applyPlugins(fs afero.Fs, p *plan.Plan) (err error) {
	apply := func(name string, plugin *plugins.CustomPlugin) error {
		log.Infof("Applying plugin %s", name)
		return errors.Wrapf(plugin.Install(fs, name), "Error applying plugin %s", name)
	}

	for pluginName, plugin := range p.Plugins.CustomPlugins {
		err = apply(pluginName, plugin)
		if err != nil {
			return err
		}
	}

	for providerName, provider := range p.Plugins.TerraformProviders {
		err = apply(providerName, provider)
		if err != nil {
			return err
		}
	}
	return
}

func applyGlobal(fs afero.Fs, p plan.Component, repoBox *packr.Box) error {
	path := fmt.Sprintf("%s/global", rootPath)
	e := fs.MkdirAll(path, 0755)
	if e != nil {
		return errors.Wrapf(e, "unable to make directory %s", path)
	}
	return applyTree(fs, repoBox, path, p)
}

func applyAccounts(fs afero.Fs, p *plan.Plan, accountBox *packr.Box) (e error) {
	for account, accountPlan := range p.Accounts {
		path := fmt.Sprintf("%s/accounts/%s", rootPath, account)
		e = fs.MkdirAll(path, 0755)
		if e != nil {
			return errors.Wrap(e, "unable to make directories for accounts")
		}
		e = applyTree(fs, accountBox, path, accountPlan)
		if e != nil {
			return errors.Wrap(e, "unable to apply templates to account")
		}
	}
	return nil
}

func applyModules(fs afero.Fs, p map[string]plan.Module, moduleBox *packr.Box) (e error) {
	for module, modulePlan := range p {
		path := fmt.Sprintf("%s/modules/%s", rootPath, module)
		e = fs.MkdirAll(path, 0755)
		if e != nil {
			return errors.Wrapf(e, "unable to make path %s", path)
		}
		e = applyTree(fs, moduleBox, path, modulePlan)
		if e != nil {
			return errors.Wrap(e, "unable to apply tree")
		}
	}
	return nil
}

func applyEnvs(fs afero.Fs, p *plan.Plan, envBox *packr.Box, componentBox *packr.Box) (e error) {
	for env, envPlan := range p.Envs {
		path := fmt.Sprintf("%s/envs/%s", rootPath, env)
		e = fs.MkdirAll(path, 0755)
		if e != nil {
			return errors.Wrapf(e, "unable to make directory %s", path)
		}
		e := applyTree(fs, envBox, path, envPlan)
		if e != nil {
			return errors.Wrap(e, "unable to apply templates to env")
		}
		for component, componentPlan := range envPlan.Components {
			path = fmt.Sprintf("%s/envs/%s/%s", rootPath, env, component)
			e = fs.MkdirAll(path, 0755)
			if e != nil {
				return errors.Wrap(e, "unable to make directories for component")
			}
			e := applyTree(fs, componentBox, path, componentPlan)
			if e != nil {
				return errors.Wrap(e, "unable to apply templates for component")
			}

			if componentPlan.ModuleSource != nil {
				e := applyModuleInvocation(fs, path, *componentPlan.ModuleSource, templates.Templates.ModuleInvocation)
				if e != nil {
					return errors.Wrap(e, "unable to apply module invocation")
				}
			}

		}
	}
	return nil
}

func applyTree(dest afero.Fs, source *packr.Box, targetBasePath string, subst interface{}) (e error) {
	return source.Walk(func(path string, sourceFile packr.File) error {
		extension := filepath.Ext(path)
		target := getTargetPath(targetBasePath, path)

		targetExtension := filepath.Ext(target)
		if extension == ".tmpl" {
			e = applyTemplate(sourceFile, dest, target, subst)
			if e != nil {
				return errors.Wrap(e, "unable to apply template")
			}
		} else if extension == ".touch" {
			e = touchFile(dest, target)
			if e != nil {
				return errors.Wrapf(e, "unable to touch file %s", target)
			}
		} else if extension == ".create" {
			e = createFile(dest, target, sourceFile)
			if e != nil {
				return errors.Wrapf(e, "unable to create file %s", target)
			}
		} else {
			log.Infof("%s copied", target)
			e = afero.WriteReader(dest, target, sourceFile)
			if e != nil {
				return errors.Wrap(e, "unable to copy file")
			}
		}

		if targetExtension == ".tf" {
			e = fmtHcl(dest, target)
			if e != nil {
				return errors.Wrap(e, "unable to format HCL")
			}
		}
		return nil
	})
}

func fmtHcl(fs afero.Fs, path string) error {
	in, e := afero.ReadFile(fs, path)
	if e != nil {
		return errors.Wrapf(e, "unable to read file %s", path)
	}
	out, e := printer.Format(in)
	if e != nil {
		return errors.Wrapf(e, "fmt hcl failed for %s", path)
	}
	return afero.WriteReader(fs, path, bytes.NewReader(out))
}

func touchFile(dest afero.Fs, path string) error {
	_, err := dest.Stat(path)
	if err != nil { // TODO we might not want to do this for all errors
		log.Infof("%s touched", path)
		_, err = dest.Create(path)
		if err != nil {
			return errors.Wrap(err, "unable to touch file")
		}
	} else {
		log.Infof("%s skipped touch", path)
	}
	return nil
}

func createFile(dest afero.Fs, path string, sourceFile io.Reader) error {
	_, err := dest.Stat(path)
	if err != nil { // TODO we might not want to do this for all errors
		log.Infof("%s created", path)
		err = afero.WriteReader(dest, path, sourceFile)
		if err != nil {
			return errors.Wrap(err, "unable to create file")
		}
	} else {
		log.Infof("%s skipped", path)
	}
	return nil
}

func removeExtension(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path))
}

func applyTemplate(sourceFile io.Reader, dest afero.Fs, path string, overrides interface{}) error {
	log.Infof("%s templated", path)
	writer, err := dest.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0755)
	if err != nil {
		return errors.Wrap(err, "unable to open file")
	}
	t := util.OpenTemplate(sourceFile)
	return t.Execute(writer, overrides)
}

// This should really be part of the plan stage, not apply. But going to
// leave it here for now and re-think it when we make this mechanism
// general purpose.
type moduleData struct {
	ModuleName   string
	ModuleSource string
	Variables    []string
	Outputs      []string
}

func applyModuleInvocation(fs afero.Fs, path, moduleAddress string, box packr.Box) error {
	e := fs.MkdirAll(path, 0755)
	if e != nil {
		return errors.Wrapf(e, "couldn't create %s directory", path)
	}

	moduleConfig, e := util.DownloadAndParseModule(moduleAddress)
	if e != nil {
		return errors.Wrap(e, "could not download or parse module")
	}

	// This should really be part of the plan stage, not apply. But going to
	// leave it here for now and re-think it when we make this mechanism
	// general purpose.
	variables := make([]string, 0)
	for _, v := range moduleConfig.Variables {
		variables = append(variables, v.Name)
	}
	sort.Strings(variables)
	outputs := make([]string, 0)
	for _, o := range moduleConfig.Outputs {
		outputs = append(outputs, o.Name)
	}
	sort.Strings(outputs)
	moduleName := filepath.Base(moduleAddress)
	re := regexp.MustCompile(`\?ref=.*`)
	moduleName = re.ReplaceAllString(moduleName, "")

	moduleAddressForSource, _ := calculateModuleAddressForSource(path, moduleAddress)
	// MAIN
	f, e := box.Open("main.tf.tmpl")
	if e != nil {
		return errors.Wrap(e, "could not open template file")
	}
	e = applyTemplate(f, fs, filepath.Join(path, "main.tf"), &moduleData{moduleName, moduleAddressForSource, variables, outputs})
	if e != nil {
		return errors.Wrap(e, "unable to apply template for main.tf")
	}
	e = fmtHcl(fs, filepath.Join(path, "main.tf"))
	if e != nil {
		return errors.Wrap(e, "unable to format main.tf")
	}

	// OUTPUTS
	f, e = box.Open("outputs.tf.tmpl")
	if e != nil {
		return errors.Wrap(e, "could not open template file")
	}

	e = applyTemplate(f, fs, filepath.Join(path, "outputs.tf"), &moduleData{moduleName, moduleAddressForSource, variables, outputs})
	if e != nil {
		return errors.Wrap(e, "unable to apply template for outputs.tf")
	}

	e = fmtHcl(fs, filepath.Join(path, "outputs.tf"))
	if e != nil {
		return errors.Wrap(e, "unable to format outputs.tf")
	}

	return nil
}

func calculateModuleAddressForSource(path, moduleAddress string) (string, error) {
	// For cases where the module is a local path, we need to calculate the
	// relative path from the component to the module.
	// The module_source path in the fogg.json is relative to the repo root.
	var moduleAddressForSource string
	// getter will kinda normalize the module address, but it will actually be
	// wrong for local file paths, so we need to calculate that ourselves below
	s, e := getter.Detect(moduleAddress, path, getter.Detectors)
	u, e := url.Parse(s)
	if e != nil || u.Scheme == "file" {
		// This indicates that we have a local path to the module.
		// It is possible that this test is unreliable.
		moduleAddressForSource, _ = filepath.Rel(path, moduleAddress)
	} else {
		moduleAddressForSource = moduleAddress
	}
	return moduleAddressForSource, nil
}
func getTargetPath(basePath, path string) string {
	target := filepath.Join(basePath, path)
	extension := filepath.Ext(path)

	if extension == ".tmpl" || extension == ".touch" || extension == ".create" {
		target = removeExtension(target)
	}

	return target
}
