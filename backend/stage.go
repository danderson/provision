package backend

import (
	"errors"
	"strings"
	"sync"
	"text/template"

	"github.com/digitalrebar/provision/backend/index"
	"github.com/digitalrebar/provision/models"
	"github.com/digitalrebar/store"
)

// Stage encapsulates tasks we want to run a machine
//
// swagger:model
type Stage struct {
	*models.Stage
	validate
	renderers       renderers
	stageParamsTmpl *template.Template
	p               *DataTracker
	rootTemplate    *template.Template
	tmplMux         sync.Mutex
}

func (obj *Stage) SetReadOnly(b bool) {
	obj.ReadOnly = b
}

func (obj *Stage) SaveClean() store.KeySaver {
	mod := *obj.Stage
	mod.ClearValidation()
	return toBackend(obj.p, nil, &mod)
}

func (s *Stage) HasTask(ts string) bool {
	for _, p := range s.Tasks {
		if p == ts {
			return true
		}
	}
	return false
}

func (s *Stage) HasProfile(name string) bool {
	for _, e := range s.Profiles {
		if e == name {
			return true
		}
	}
	return false
}

func (s *Stage) Indexes() map[string]index.Maker {
	fix := AsStage
	res := index.MakeBaseIndexes(s)
	res["Name"] = index.Make(
		true,
		"string",
		func(i, j models.Model) bool {
			return fix(i).Name < fix(j).Name
		},
		func(ref models.Model) (gte, gt index.Test) {
			name := fix(ref).Name
			return func(ss models.Model) bool {
					return fix(ss).Name >= name
				},
				func(ss models.Model) bool {
					return fix(ss).Name > name
				}
		},
		func(ss string) (models.Model, error) {
			res := fix(s.New())
			res.Name = ss
			return res, nil
		})
	res["BootEnv"] = index.Make(
		false,
		"string",
		func(i, j models.Model) bool {
			return fix(i).BootEnv < fix(j).BootEnv
		},
		func(ref models.Model) (gte, gt index.Test) {
			bootenv := fix(ref).BootEnv
			return func(ss models.Model) bool {
					return fix(ss).BootEnv >= bootenv
				},
				func(ss models.Model) bool {
					return fix(ss).BootEnv > bootenv
				}
		},
		func(ss string) (models.Model, error) {
			res := fix(s.New())
			res.BootEnv = ss
			return res, nil
		})
	res["Reboot"] = index.Make(
		false,
		"boolean",
		func(i, j models.Model) bool {
			return !fix(i).Reboot && fix(j).Reboot
		},
		func(ref models.Model) (gte, gt index.Test) {
			reboot := fix(ref).Reboot
			return func(s models.Model) bool {
					v := fix(s).Reboot
					return v || (v == reboot)
				},
				func(s models.Model) bool {
					return fix(s).Reboot && !reboot
				}
		},
		func(ss string) (models.Model, error) {
			res := fix(s.New())
			switch ss {
			case "true":
				res.Reboot = true
			case "false":
				res.Reboot = false
			default:
				return nil, errors.New("Reboot must be true or false")
			}
			return res, nil
		})
	return res
}

func (s *Stage) Backend() store.Store {
	return s.p.getBackend(s)
}

func (s *Stage) genRoot(commonRoot *template.Template, e models.ErrorAdder) *template.Template {
	res := models.MergeTemplates(commonRoot, s.Templates, e)
	for i, tmpl := range s.Templates {
		if tmpl.Path == "" {
			e.Errorf("Template[%d] needs a Path", i)
		}
	}
	if s.HasError() != nil {
		return nil
	}
	return res
}

func (s *Stage) Validate() {
	if strings.Contains(s.Name, "/") || strings.Contains(s.Name, "\\") {
		s.Errorf("Name must not contain a '/' or '\\'")
	}
	s.renderers = renderers{}
	// First, the stuff that must be correct in order for
	s.AddError(index.CheckUnique(s, s.stores("stages").Items()))
	for _, taskName := range s.Tasks {
		if s.stores("tasks").Find(taskName) == nil {
			s.Errorf("Task %s does not exist", taskName)
		}
	}
	for _, profileName := range s.Profiles {
		if s.stores("profiles").Find(profileName) == nil {
			s.Errorf("Profile %s does not exist", profileName)
		}
	}
	if s.BootEnv != "" {
		if nbFound := s.stores("bootenvs").Find(s.BootEnv); nbFound == nil {
			s.Errorf("BootEnv %s does not exist", s.BootEnv)
		} else {
			env := AsBootEnv(nbFound)
			if !env.Available {
				s.Errorf("Stage %s wants BootEnv %s, which is not available", s.Name, s.BootEnv)
			} else {
				for _, ti := range env.Templates {
					for _, si := range s.Templates {
						if si.Path == ti.Path {
							s.Errorf("Stage %s Template %s overlaps with BootEnv %s Template %s",
								s.Name, si.Name, s.BootEnv, ti.Name)
						}
					}
				}
			}
		}
	}
	// If our basic templates do not parse, it is game over for us
	s.p.tmplMux.Lock()
	s.tmplMux.Lock()
	root := s.genRoot(s.p.rootTemplate, s)
	s.p.tmplMux.Unlock()
	if root != nil {
		s.rootTemplate = root
	}
	s.tmplMux.Unlock()
	if !s.SetValid() {
		// If we have not been validated at this point, return.
		return
	}
	// Update renderers on machines
	machines := s.stores("machines")
	if machines != nil {
		for _, i := range machines.Items() {
			machine := AsMachine(i)
			if machine.Stage != s.Name {
				continue
			}
			s.renderers = append(s.renderers, s.Render(s.stores, machine, s)...)
		}
	}
	s.SetAvailable()
}

func (s *Stage) OnLoad() error {
	s.stores = func(ref string) *Store {
		return s.p.objs[ref]
	}
	defer func() { s.stores = nil }()
	return s.BeforeSave()
}

func (s *Stage) New() store.KeySaver {
	res := &Stage{Stage: &models.Stage{}}
	if s.Stage != nil && s.ChangeForced() {
		res.ForceChange()
	}
	res.p = s.p
	res.Profiles = []string{}
	res.Tasks = []string{}
	res.Templates = []models.TemplateInfo{}
	return res
}

func (s *Stage) setDT(p *DataTracker) {
	s.p = p
}

func (s *Stage) BeforeSave() error {
	if s.Profiles == nil {
		s.Profiles = []string{}
	}
	if s.Tasks == nil {
		s.Tasks = []string{}
	}
	if s.Templates == nil {
		s.Templates = []models.TemplateInfo{}
	}
	s.Validate()
	if !s.Validated {
		return s.MakeError(422, ValidationError, s)
	}
	return nil
}

func (s *Stage) BeforeDelete() error {
	e := &models.Error{Code: 409, Type: StillInUseError, Object: s}
	machines := s.stores("machines")
	for _, i := range machines.Items() {
		machine := AsMachine(i)
		if machine.Stage != s.Name {
			continue
		}
		e.Errorf("Stage %s in use by Machine %s", s.Name, machine.Name)
	}
	return e.HasError()
}

func AsStage(o models.Model) *Stage {
	return o.(*Stage)
}

func AsStages(o []models.Model) []*Stage {
	res := make([]*Stage, len(o))
	for i := range o {
		res[i] = AsStage(o[i])
	}
	return res
}

func (s *Stage) renderInfo() ([]models.TemplateInfo, []string) {
	return s.Templates, s.RequiredParams
}

func (s *Stage) templates() *template.Template {
	return s.rootTemplate
}

func (s *Stage) Render(d Stores, m *Machine, e models.ErrorAdder) renderers {
	if len(s.RequiredParams) > 0 && m == nil {
		e.Errorf("Machine is nil or does not have params")
		return nil
	}
	r := newRenderData(d, s.p, m, s)
	return r.makeRenderers(e)
}

func (s *Stage) AfterSave() {
	if s.Available && s.renderers != nil {
		s.renderers.register(s.p.FS)
	}
	s.renderers = nil
}

var stageLockMap = map[string][]string{
	"get":    []string{"stages"},
	"create": []string{"stages", "bootenvs", "machines", "tasks", "templates", "profiles"},
	"update": []string{"stages", "bootenvs", "machines", "tasks", "templates", "profiles"},
	"patch":  []string{"stages", "bootenvs", "machines", "tasks", "templates", "profiles"},
	"delete": []string{"stages", "bootenvs", "machines", "tasks", "templates", "profiles"},
}

func (s *Stage) Locks(action string) []string {
	return stageLockMap[action]
}
