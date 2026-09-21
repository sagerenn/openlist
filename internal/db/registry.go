package db

// registrar holds model-registration functions supplied by feature
// packages via init(). This breaks the import cycle: feature packages
// import db (to register and to use DB()), and db never imports them.

var registrars []func() []interface{}

// RegisterModels is called by feature packages from an init() to supply
// their gorm model types for auto-migration.
func RegisterModels(fn func() []interface{}) {
	registrars = append(registrars, fn)
}

// collectModels gathers all registered models into Models. Called by Init.
func collectModels() {
	Models = nil
	for _, fn := range registrars {
		Models = append(Models, fn()...)
	}
}
