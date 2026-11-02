// Package bootstrap 定义装配用的依赖集合（TT-T-06/07）。
//
// 来源系统这里按配置连 PostgreSQL、Redis、RabbitMQ、S3；tabtv 是单进程桌面应用：
// 数据库是内嵌 PG、事件总线与队列在进程内、对象存储默认是本地目录。依赖由 desktop 包直接构造好填进来。
package bootstrap

import (
	"github.com/li904338203/tabtv/backend/internal/platform/db"
	"github.com/li904338203/tabtv/backend/internal/platform/eventbus"
	"github.com/li904338203/tabtv/backend/internal/platform/objstore"
)

// Deps 持有已建立的依赖。
type Deps struct {
	DB       *db.Pool
	Bus      *eventbus.Local
	ObjStore *objstore.Client
}
