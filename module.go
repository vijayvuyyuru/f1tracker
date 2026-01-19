package f1viz

import (
	"context"
	"errors"
	"fmt"

	"github.com/golang/geo/r3"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
	generic "go.viam.com/rdk/services/generic"
)

var (
	F1viz            = resource.NewModel("vijayvuyyuru", "viz", "f1viz")
	errUnimplemented = errors.New("unimplemented")
	startPoint       = r3.Vector{X: -641,
		Y: -922,
		Z: 1303,
	}
)

const (
	circuitKey  = 9
	sessionName = "Race"
)

func init() {
	resource.RegisterService(generic.API, F1viz,
		resource.Registration[resource.Resource, *Config]{
			Constructor: newVizF1viz,
		},
	)
}

type Config struct {
	Board string `json:"board"`
}

// Validate ensures all parts of the config are valid and important fields exist.
// Returns three values:
//  1. Required dependencies: other resources that must exist for this resource to work.
//  2. Optional dependencies: other resources that may exist but are not required.
//  3. An error if any Config fields are missing or invalid.
//
// The `path` parameter indicates
// where this resource appears in the machine's JSON configuration
// (for example, "components.0"). You can use it in error messages
// to indicate which resource has a problem.
func (cfg *Config) Validate(path string) ([]string, []string, error) {
	// Add config validation code here
	return nil, nil, nil
}

type vizF1viz struct {
	resource.AlwaysRebuild

	name resource.Name

	logger logging.Logger
	cfg    *Config

	cancelCtx  context.Context
	cancelFunc func()
}

func newVizF1viz(ctx context.Context, deps resource.Dependencies, rawConf resource.Config, logger logging.Logger) (resource.Resource, error) {
	conf, err := resource.NativeConfig[*Config](rawConf)
	if err != nil {
		return nil, err
	}

	return NewF1viz(ctx, deps, rawConf.ResourceName(), conf, logger)

}

func NewF1viz(ctx context.Context, deps resource.Dependencies, name resource.Name, conf *Config, logger logging.Logger) (resource.Resource, error) {

	cancelCtx, cancelFunc := context.WithCancel(context.Background())

	s := &vizF1viz{
		name:       name,
		logger:     logger,
		cfg:        conf,
		cancelCtx:  cancelCtx,
		cancelFunc: cancelFunc,
	}
	return s, nil
}

func (s *vizF1viz) Name() resource.Name {
	return s.name
}

func (s *vizF1viz) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	return nil, fmt.Errorf("not implemented")
}

func (s *vizF1viz) Close(context.Context) error {
	// Put close code here
	s.cancelFunc()
	return nil
}
