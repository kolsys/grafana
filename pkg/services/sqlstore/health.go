package sqlstore

import (
	"github.com/p0hil/grafana/pkg/bus"
	m "github.com/p0hil/grafana/pkg/models"
)

func init() {
	bus.AddHandler("sql", GetDBHealthQuery)
}

func GetDBHealthQuery(query *m.GetDBHealthQuery) error {
	return x.Ping()
}
