package helper

import (
	"github.com/bluemedora/bplogagent/errors"
	"github.com/bluemedora/bplogagent/plugin"
)

// OutputConfig provides a basic implementation of an output plugin config.
type OutputConfig struct {
	BasicConfig `mapstructure:",squash" yaml:",inline"`
}

// ID will return the plugin id.
func (c OutputConfig) ID() string {
	return c.PluginID
}

// Type will return the plugin type.
func (c OutputConfig) Type() string {
	return c.PluginType
}

// Build will build an output plugin.
func (c OutputConfig) Build(context plugin.BuildContext) (OutputPlugin, error) {
	basicPlugin, err := c.BasicConfig.Build(context)
	if err != nil {
		return OutputPlugin{}, err
	}

	outputPlugin := OutputPlugin{
		BasicPlugin: basicPlugin,
	}

	return outputPlugin, nil
}

// SetNamespace will namespace the id and output of the plugin config.
func (c *OutputConfig) SetNamespace(namespace string, exclusions ...string) {
	if CanNamespace(c.PluginID, exclusions) {
		c.PluginID = AddNamespace(c.PluginID, namespace)
	}
}

// OutputPlugin provides a basic implementation of an output plugin.
type OutputPlugin struct {
	BasicPlugin
}

// CanProcess will always return true for an output plugin.
func (o *OutputPlugin) CanProcess() bool {
	return true
}

// CanOutput will always return false for an output plugin.
func (o *OutputPlugin) CanOutput() bool {
	return false
}

// Outputs will always return an empty array for an output plugin.
func (o *OutputPlugin) Outputs() []plugin.Plugin {
	return []plugin.Plugin{}
}

// SetOutputs will return an error if called.
func (o *OutputPlugin) SetOutputs(plugins []plugin.Plugin) error {
	return errors.NewError(
		"Plugin can not output, but is attempting to set an output.",
		"This is an unexpected internal error. Please submit a bug/issue.",
	)
}