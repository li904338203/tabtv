package wiring

// 时间线服务的装配（P10-01 起写在 cmd/api，AGT-50 抽到这里）：画布 Agent 的 timeline_assemble /
// render 在 Worker 里调同一个服务。两个进程各抄一份适配器的话，「渲染多少钱」「加不加水印」
// 早晚有两个答案，而且没有编译错误。

import (
	"context"
	"log/slog"
	"time"

	"github.com/li904338203/tabtv/backend/internal/platform/ffbin"

	"github.com/jackc/pgx/v5/pgxpool"

	billingrepo "github.com/li904338203/tabtv/backend/internal/billing/repo"
	canvasapp "github.com/li904338203/tabtv/backend/internal/canvas/app"
	catalogapp "github.com/li904338203/tabtv/backend/internal/catalog/app"
	catalogdomain "github.com/li904338203/tabtv/backend/internal/catalog/domain"
	identityrepo "github.com/li904338203/tabtv/backend/internal/identity/repo"
	mediaapp "github.com/li904338203/tabtv/backend/internal/media/app"
	"github.com/li904338203/tabtv/backend/internal/platform/config"
	scriptapp "github.com/li904338203/tabtv/backend/internal/script/app"
	timelineapp "github.com/li904338203/tabtv/backend/internal/timeline/app"
	timelinedomain "github.com/li904338203/tabtv/backend/internal/timeline/domain"
	timelinerepo "github.com/li904338203/tabtv/backend/internal/timeline/repo"
)

// TimelineInput 是 NewTimelineService 的入参。
type TimelineInput struct {
	Pool    *pgxpool.Pool
	NextID  func() int64
	Media   *mediaapp.Service
	Script  *scriptapp.Service
	Canvas  *canvasapp.Service
	Catalog *catalogapp.Service
	// CompensationValidFor 渲染退款时过期批次的补偿有效期（同生成域）。
	CompensationValidFor time.Duration
	Logger               *slog.Logger
	// Smartcut 导出限流与下载链接有效期（SCT-15）。
	Smartcut config.Smartcut
	// Events 时间线写入后发 timeline.updated（docs/agent/smartcut SCD-19）。API 与 Worker 各接一份；
	// 为 nil（没有 Redis）时只是没有实时信号，前端按版本号补取。
	Events timelineapp.EventPublisher
}

// NewTimelineService 装出时间线服务（含渲染受理：预检 → 报价 → 扣费 → 写任务 + outbox）。
//
// ⚠️ 它按**接口**拿素材与镜头，不直接读媒体域 / 脚本域的表 —— 直接读的话，
// 「这份素材属不属于这个工作区」这条会在两个域各判一遍，而两遍迟早不一致。
func NewTimelineService(in TimelineInput) *timelineapp.Service {
	tl := timelineapp.New(timelinerepo.New(in.Pool, in.NextID))
	tl.SetMedia(timelineMedia{in.Media})
	tl.SetShots(timelineShots{in.Script})
	tl.SetProject(NewProjectGuard(in.Canvas))
	// 渲染受理（P10-03）。⚠️ 报价走**同一套定价目录**，不另写一份价格。
	tl.SetBilling(in.Pool, billingrepo.New(in.Pool, in.NextID))
	tl.SetRenderQuoter(RenderQuoter{in.Catalog})
	tl.SetRefunder(renderRefunder{billingrepo.New(in.Pool, in.NextID), in.CompensationValidFor})
	tl.SetNextID(in.NextID)
	// ⚠️ 本机有没有 ffmpeg 在**受理时**就判：不判的话任务会排队然后失败，
	// 而错误信息里只有一句 "exec: not found"。
	tl.SetFFmpegAvailable(timelineapp.FFmpegAvailable())
	tl.SetEncoderAvailable(func() bool { return ffbin.H264().Encoder.Name != "" })
	// 水印开关（GAP-13）。⚠️ **服务端按用户 ID 查库**，不从请求体收：
	// 让客户端自己报"我关了水印"等于把 AI 标识变成谁都能关的可选项。
	tl.SetUserFlags(userWatermarkFlags{identityrepo.New(in.Pool)})
	// 水印图是**平台配置**、不属于任何工作区，所以按 ID 直查存在性，
	// 不走按工作区解析的那条（那条一定查不到）。
	tl.SetMediaExistence(platformMedia{in.Media})
	tl.SetLogger(in.Logger)
	// 导出（docs/agent/smartcut SCT-15）：免费、按用户限流；成片走 5 分钟的签名下载链接。
	tl.SetRenderLimits(timelineapp.RenderLimits{UserConcurrency: in.Smartcut.RenderUserConcurrency,
		UserPerHour: in.Smartcut.RenderUserPerHour})
	tl.SetRenderDownloads(in.Media, in.Smartcut.RenderDownloadTTL)
	if in.Events != nil {
		tl.SetEvents(in.Events)
	}
	return tl
}

// timelineMedia 把媒体域接成时间线的素材端口。
//
// ⚠️ 只回"是什么类型、多长"，**不回 bucket / objectKey**：
// 时间线拿不到定位信息就绕不过受保护读取（GO-27）。
type timelineMedia struct{ svc *mediaapp.Service }

func (a timelineMedia) Lookup(ctx context.Context, ws, mediaID int64) (
	timelineapp.MediaInfo, error) {
	m, err := a.svc.Info(ctx, ws, mediaID)
	if err != nil {
		return timelineapp.MediaInfo{}, err
	}
	return timelineapp.MediaInfo{
		Kind: mediaapp.KindOf(m.ContentType), DurationMs: m.DurationMs,
	}, nil
}

// timelineShots 把脚本域接成"按分镜装配"的来源。
//
// ⚠️ 缺口的**原因**由脚本域给：只有它知道那一镜是没加进视频组、
// 还是生成失败了、还是正在跑。时间线这边自己编一句的话，
// 用户照着那句话去修反而修不好（NEW-09）。
type timelineShots struct{ svc *scriptapp.Service }

func (a timelineShots) ShotClips(ctx context.Context, ws, scriptID int64) (
	[]timelinedomain.ShotClip, error) {
	rows, err := a.svc.ShotVideos(ctx, ws, scriptID)
	if err != nil {
		return nil, err
	}
	out := make([]timelinedomain.ShotClip, 0, len(rows))
	for _, v := range rows {
		out = append(out, timelinedomain.ShotClip{
			ShotID: v.ShotID, ShotNo: v.ShotNo, MediaID: v.MediaID, Reason: v.Reason,
		})
	}
	return out, nil
}

// RenderQuoter 把目录域的定价接成渲染报价端口（也供画布 Agent 的规划报价复用，同一个口径）。
//
// ⚠️ 走 `timeline_render` 这个 operation 的定价（per_second）：
// 与图片/视频生成共用同一套目录，运营在同一个页面上调。
type RenderQuoter struct{ svc *catalogapp.Service }

func (q RenderQuoter) QuoteRender(ctx context.Context, durationSec int,
	memberLevelID int64) (int, error) {
	// ⚠️ 走 `QuoteOperation` 而不是 `Quote`：渲染**没有模型**，
	// 而 `Quote` 是从 model×operation 的绑定开始的 ——
	// 没有模型时第一步就报「该模型不支持操作 timeline_render」，
	// 而那句话对渲染完全不成立。
	//
	// ⚠️ 会员等级暂不参与渲染报价（P11 定稿会员权益时再接）：
	// 现在传进来也没有地方用它，留着参数是为了那时不用改签名。
	_ = memberLevelID
	return q.svc.QuoteOperation(ctx, catalogdomain.OpTimelineRender, 1, durationSec)
}

// renderRefunder 把账本接成渲染的退款端口。
type renderRefunder struct {
	repo     *billingrepo.Repo
	validFor time.Duration
}

func (a renderRefunder) Refund(ctx context.Context, payerID int64,
	refType, refID string, amount int, reason string) error {
	// ⚠️ 退款金额由**账本按原消费流水**算，不由调用方报：
	// 报一个数的话，调用方算错就直接多退或少退（退款按原批次回补，
	// 而"原批次"只有账本知道）。amount 只用于日志对照。
	_ = amount
	_, err := a.repo.Refund(ctx, billingrepo.RefundInput{
		PayerID: payerID, RefType: refType, RefID: refID,
		Reason: reason, CompensationValidFor: a.validFor,
	})
	return err
}

// userWatermarkFlags 把身份域接成"渲染要不要加水印"的来源。
//
// ⚠️ 这是**服务端解析**：开关的唯一事实来源是 `app_user.is_ai_watermark_off`，
// 不是请求体里的一个布尔（GO-27 同一条）。
type userWatermarkFlags struct{ repo *identityrepo.Repo }

func (a userWatermarkFlags) WatermarkOff(ctx context.Context, userID int64) (bool, error) {
	u, err := a.repo.FindByID(ctx, userID)
	if err != nil {
		return false, err
	}
	return u.IsWatermarkOff, nil
}

// platformMedia 把媒体域接成**平台级**素材的存在性校验（水印图）。
type platformMedia struct{ svc *mediaapp.Service }

func (a platformMedia) Exists(ctx context.Context, mediaID int64) error {
	_, err := a.svc.Get(ctx, mediaID)
	return err
}
