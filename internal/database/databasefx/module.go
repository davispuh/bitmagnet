package databasefx

import (
	"github.com/bitmagnet-io/bitmagnet/internal/boilerplate/config/configfx"
	"github.com/bitmagnet-io/bitmagnet/internal/database"
	"github.com/bitmagnet-io/bitmagnet/internal/database/cache"
	"github.com/bitmagnet-io/bitmagnet/internal/database/dao"
	"github.com/bitmagnet-io/bitmagnet/internal/database/healthcheck"
	"github.com/bitmagnet-io/bitmagnet/internal/database/migrations"
	"github.com/bitmagnet-io/bitmagnet/internal/database/postgres"
	"github.com/bitmagnet-io/bitmagnet/internal/database/search"
	"github.com/bitmagnet-io/bitmagnet/internal/database/search/warmer"
	"go.uber.org/fx"
)

func New() fx.Option {
	return fx.Module(
		"database",
		configfx.NewConfigModule[postgres.Config]("postgres", postgres.NewDefaultConfig()),
		configfx.NewConfigModule[cache.Config]("gorm_cache", cache.NewDefaultConfig()),
		configfx.NewConfigModule[warmer.Config]("search_warmer", warmer.NewDefaultConfig()),
		fx.Provide(
			cache.NewInMemoryCacher,
			cache.NewPlugin,
			dao.New,
			database.New,
			healthcheck.New,
			migrations.New,
			postgres.New,
			search.New,
			warmer.New,
		),
		fx.Decorate(
			cache.NewDecorator,
		),
	)
}
