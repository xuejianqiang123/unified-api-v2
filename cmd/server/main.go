// main.go workbuddy2api 入口：加载配置、构建 pool、起调度器与 HTTP 服务。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"unified-api-v2/internal/alert"
	"unified-api-v2/internal/auth"
	codeartspkg "unified-api-v2/internal/codearts"
	"unified-api-v2/internal/dbconfig"
	"unified-api-v2/internal/dbserver"
	"unified-api-v2/internal/pool"
	"unified-api-v2/internal/redisstore"
	"unified-api-v2/internal/scheduler"
	"unified-api-v2/internal/server"
	"unified-api-v2/internal/session"
	"unified-api-v2/internal/upstream"
)

// checkinReportFn 把 scheduler 的**进程内**签到接到 HTTP 入口（POST /v1/checkin）。
//
// ★ 直接调 CheckinAll，而不是 exec deploy/signin ★ 号池的 credits 只由 CheckinAll
// 这条路径写入（SetCreditsDetailed）；外部 CLI 虽然能签到成功，但那是另一个进程，
// 网关内存里的额度不会更新，控制台照样显示旧值 —— 这正是 2026-09-22 用户报的
// 「额度没刷新」。详见 internal/server/checkin.go。
func checkinReportFn(sch *scheduler.Scheduler, p *pool.Pool, cfg *Config) func() (server.CheckinReport, bool, error) {
	return func() (server.CheckinReport, bool, error) {
		outcomes, err := sch.CheckinAll()
		if errors.Is(err, scheduler.ErrBusy) {
			// 手动入口与定时撞车：不是错误，交给 handler 回 429 busy。
			return server.CheckinReport{}, true, nil
		}
		if err != nil {
			return server.CheckinReport{}, false, err
		}
		// realm 由号池现查（CheckinOutcome 本身不带 realm，免得 scheduler 与 pool
		// 两处各存一份口径）；查不到留空，前端按「—」显示。
		return buildCheckinReport(outcomes, realmOfFn(p),
			cfg.Schedule.CheckinEnabled, cfg.Schedule.CheckinHours), false, nil
	}
}

// realmOfFn 返回「按 uid 现查 realm」的闭包。手动回执与签到历史落盘共用它，
// 保证两条路径的 realm 归属永远一致。
func realmOfFn(p *pool.Pool) func(string) string {
	return func(uid string) string {
		if a := p.AuthByUID(uid); a != nil {
			return a.Realm()
		}
		return ""
	}
}

// buildCheckinReport 把 scheduler 的结果映射成 HTTP 响应体（纯函数，便于单测：
// 计数与 realm 归属错了不会报错，只会让面板显示错，必须锁住）。
func buildCheckinReport(outcomes []scheduler.CheckinOutcome, realmOf func(string) string,
	enabled bool, hours []int) server.CheckinReport {
	rep := server.CheckinReport{
		Enabled: enabled,
		Hours:   hours,
		Total:   len(outcomes),
		Results: make([]server.CheckinResult, 0, len(outcomes)),
	}
	for _, o := range outcomes {
		rep.Results = append(rep.Results, server.CheckinResult{
			UID:      o.UID,
			Nickname: o.Nickname,
			Realm:    realmOf(o.UID),
			Status:   string(o.Status),
			Credits:  o.Credits,
			Detail:   o.Detail,
		})
		switch o.Status {
		case scheduler.CheckinOK:
			rep.OK++
		case scheduler.CheckinAlready:
			rep.Already++
		case scheduler.CheckinFail:
			rep.Fail++
		case scheduler.CheckinSkipped:
			rep.Skipped++
		}
	}
	return rep
}

// modelJSONPath 由 state.json 路径推导 model.json 路径（同目录同名换缀）：
// 两者同为数据目录持久化物（Docker ./data volume），配套而非各自配置。
// state 路径为空（纯内存测试形态）→ 空 = 禁用 model.json 落盘（内存 + 种子仍可用）。
func modelJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "model.json")
}

// checkinJSONPath 由 state.json 路径推导 checkin.json 路径（同目录同名换缀）：
// 签到历史与 state.json 同为数据目录持久化物（Docker ./data volume），配套而非各自配置。
// 空 state 路径（纯内存测试形态）→ 空 = 禁用落盘（历史仅存内存）。
func checkinJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "checkin.json")
}

// providerOrderJSONPath 由 state.json 路径推导 provider_order.json 路径（同目录同名换缀）：
// 「自动路由的 Provider 优先序」是与 state 同级的运维偏好，独立存放、不写 config.json。
func providerOrderJSONPath(stateFile string) string {
	if stateFile == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(stateFile), "provider_order.json")
}

// runServer 是原来的 main 主体：解析配置、装配 provider、启动 HTTP 服务。
// 由 main 在「普通前台运行」与「Windows 服务」两种模式下调用。
func runServer() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	// -detach 由 main 在进入 runServer 之前处理，这里必须声明：
	// flag 包遇到未定义参数会直接 Exit(2)，网关会静默起不来。
	_ = flag.Bool("detach", false, "detach from console (used by Task Scheduler)")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		// 配置文件不存在时给一次机会用纯默认 + env
		if os.IsNotExist(err) {
			log.Printf("config %s not found, using defaults+env", *cfgPath)
			cfg, err = Load("")
		}
		if err != nil {
			log.Fatalf("load config: %v", err)
		}
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	// global realm 路由开关（config global.enabled，缺省 true）：注入 auth 包全局闸。
	// Realm()/IsGlobal() 先过此闸——显式 false 时恒 cn（逃生门：纯 CN 锁定的第一道闸）。
	auth.SetGlobalEnabled(cfg.Global.Enabled)

	// model.json 本地缓存接线（context_length 四级查找链第 3 级）：数据目录与
	// state.json 同风格（Docker volume 持久化路径 ./data）。首次缺失/损坏自动回落
	// 仓库种子 embed；models.dev 按需拉取成功后原子写回。
	upstream.SetModelCatalogPath(modelJSONPath(cfg.StateFile))

	// redisstore：未配置/连接失败 → Noop（纯内存模式，一切功能照常）。
	store := redisstore.New(cfg.Upstash.URL, cfg.Upstash.Token)

	p := pool.New(cfg.StateFile)
	defer p.Close() // 进程退出前停后台落盘 goroutine + 最后补一次落盘（FIX-4:goroutine 泄漏）
	p.SetStore(store)
	p.RestoreFromSnapshot() // 择新恢复：Redis 快照比本地新才采用，否则本地优先
	p.SyncToDir(auths)      // 与 auths 目录对齐：新账号加入、已删除文件账号剔除（状态保留）

	// auths 目录热加载：新增凭证文件自动进池，免去「加完账号手动重启网关」。
	// 启动时的 SyncToDir 已建立基线，监听只在后续目录内容变化时触发（见 pool/watch.go）。
	stopWatch := p.StartAuthDirWatch(cfg.AuthDir)
	defer stopWatch()

	// 熔断器 + 在途上限 + 三因子加权调优（从 config 注入，非正值回退默认）。
	p.SetBreaker(cfg.Pool.BreakerThreshold, cfg.BreakerCooldownDur, cfg.BreakerCooldownMaxD)
	// 连败降权（issue #114）：ErrClient/传输层连败 N 次临时出池。
	p.SetDegrade(cfg.Pool.DegradeThreshold, cfg.DegradeCooldownDur, cfg.DegradeCooldownMaxD)
	p.SetMaxInFlight(cfg.Pool.MaxInFlight)
	p.SetMaxInFlightGlobal(cfg.Pool.MaxInFlightGlobal) // global 域在途分档（WAF 403 修复 P1-1，默认 2）
	p.SetSoftRateMax(cfg.SoftRateMaxDur)               // 软冷却指数退避封顶（soft_rate_max，默认 2h）
	// 积分耗尽后的冷却时长（cooldown.credit_exhausted，issue #78）：0 = 保持
	// 「硬冷却到次日 04:00」旧行为；配了就只退出这一小段再回候选池。
	p.SetCreditExhausted(cfg.CreditExhaustedDur)
	p.SetWeights(cfg.Pool.IdleWeightPerHour, cfg.Pool.IdleWeightMax)
	p.SetCostExploreInterval(cfg.CostExploreIntervalDur) // costTier 探索窗口（issue #136，默认 30m；0 关停）
	p.SetSelectionMode(cfg.Pool.SelectionMode)           // 选号模式：weighted / priority（H5 拖拽排序）

	// 会话粘性路由（可配关闭）。
	var sessRouter *session.Router
	redisMode := "noop"
	if _, ok := store.(redisstore.Noop); !ok {
		redisMode = "upstash"
	}
	if cfg.SessionSticky.Enabled {
		sessRouter = session.New(session.Config{
			TTL:        cfg.SessionTTL,
			GCInterval: cfg.SessionGCInterval,
			Store:      store,
			Available:  p.AvailableUIDs,
			// 按模型的可用性口径：绑定号在当前模型被 6004 限额时重分配，
			// 而不是被钉在这个号上反复失败。
			// realm 感知闭包：带前缀模型名按 realm 过滤可用账号（跨 realm 不泄漏，
			// 见 wiring.go）；裸名走 cn（现状零回归）。
			AvailableForModel: realmAwareAvailableForModel(p),
		})
		sessRouter.LoadFromStore() // 启动时从 Redis 恢复粘性（读操作仅此处）
		sessRouter.StartGC()
		defer sessRouter.StopGC()
	}
	sessCount := func() int {
		if sessRouter != nil {
			return sessRouter.Count()
		}
		return 0
	}

	up := upstream.New()
	// 短 RPC 总时长上限（refresh/checkin/balance/FetchModels），语义不变。
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 聊天 SSE 首字节前（响应头）上限：cfg 已 normalize（缺省回落 timeout_seconds）。
	up.HeaderTimeout = time.Duration(cfg.Upstream.HeaderTimeoutSeconds) * time.Second
	if tr, ok := up.ChatHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = up.HeaderTimeout
	}
	// 聊天 SSE 流中空闲上限（S3 空闲监控读取）。
	up.IdleTimeout = time.Duration(cfg.Upstream.IdleTimeoutSeconds) * time.Second
	up.SanitizeFingerprints = cfg.Features.SanitizeBlacklistFingerprints
	// 出站 UA（A 段）：非空才做显式覆盖，空 = 默认 WorkBuddy 三段式
	// `WorkBuddy/<client_version> WorkBuddy/<client_version> CLI/<cli_version>`。
	up.UserAgent = cfg.Upstream.UserAgent
	// 版本段（upstream.client_version / cli_version）：空 = 各走内置默认。
	up.ClientVersion = cfg.Upstream.ClientVersion
	up.CliVersion = cfg.Upstream.CliVersion
	// 设备风控头（X-Device-Token）全局兜底 + 文件读取路径；空 = 不注入。
	up.DeviceToken = cfg.Upstream.DeviceToken
	up.DeviceTokenFile = cfg.Upstream.DeviceTokenFile
	// 用量归属头（X-Product/X-IDE-*）+ 客户端 IP 透传开关（见 ChatHeaders / handler）。
	up.ClientName = cfg.Upstream.ClientName
	up.PassthroughIP = cfg.Upstream.PassthroughIP
	// global realm 双域路由（config global 段）：base 空回落内置默认 https://www.workbuddy.ai；
	// GlobalEnabled 与 auth 包开关一致（双保险第二道闸在 upstream.globalOn）。
	up.ChatBaseGlobal = cfg.Global.ChatBase
	up.BillingBaseGlobal = cfg.Global.BillingBase
	up.GlobalEnabled = cfg.Global.Enabled

	// 签到历史落盘（GET /v1/checkin/history 的数据源）。此前签到结果只活在
	// POST /v1/checkin 的 HTTP 响应里——刷新页面即失忆，自动 9/21 那趟更是完全
	// 看不到（日志只有一行聚合计数，成功路径不打日志）。文件缺失 = 还没有历史
	// （不报错）；解析失败改名留证再从空开始，绝不静默覆盖。
	checkinStore := server.NewCheckinHistoryStore(checkinJSONPath(cfg.StateFile))
	checkinStore.Load()

	// scheduleSeed 部署私有盐：让 random_delay_minutes 的每日随机时刻在重启后稳定、
	// 且不同部署错开（上游用 APIKey 或 AuthDir 兜底——两者都已具备、无需新增熵源；
	// 绝不出现在日志里）。
	scheduleSeed := cfg.APIKey
	if scheduleSeed == "" {
		scheduleSeed = cfg.AuthDir
	}
	// CodeArts：进程内 provider，凭据为华为 STS 四件套 + DPoP 私钥。
	// 凭据缺失不阻止启动（别名校验只要求名字合法），请求时回 503 并说明缺什么。
	var codeArtsProvider *server.CodeArtsProvider
	codeArtsCredPath := "codearts/accounts.json"
	if cfg.Providers.CodeArts.Enabled {
		credPath := cfg.Providers.CodeArts.Accounts
		if credPath == "" {
			credPath = "codearts/accounts.json"
		}
		codeArtsCredPath = credPath
		codeArtsProvider = server.NewCodeArtsProvider(credPath)
		log.Printf("codearts provider 已接线（凭据 %s）", credPath)
	}

	// CodeArts 凭据自愈：STS 剩余不足 2 小时就从 IDE 重读一次。
	//
	// 前提是 IDE 自己在续期（它确实会，实测 14:29 → 16:24）；IDE 没开时刷新会失败，
	// 此时**保留旧凭据**而不是清空 —— 还能用的东西不该因为刷新失败被丢掉。
	if codeArtsProvider != nil {
		go func() {
			t := time.NewTicker(30 * time.Minute)
			defer t.Stop()
			for range t.C {
				cli := codeArtsProvider.Client()
				if !cli.NeedsIDERefresh() {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				var rerr error
				if cfg.Providers.CodeArts.Takeover {
					// 接管模式：网关自己用 refresh_token 换票（IDE 那份随之失效）。
					rerr = cli.RefreshOwn(ctx)
				} else {
					// 只读模式：从 IDE 已续好的凭据里抄一份。
					rerr = cli.RefreshFromIDE()
				}
				cancel()
				if rerr != nil {
					log.Printf("codearts 凭据自愈失败（保留旧凭据）: %v", rerr)
					continue
				}
				log.Printf("codearts 凭据已自动刷新（takeover=%v）", cfg.Providers.CodeArts.Takeover)
			}
		}()
	}

	// CodeArts 签到的唯一出口：scheduler 的自动排程与面板的「立即签到」都调它，
	// 两条路径同一份判定（避免面板说已领、排程说失败）。
	codeArtsCheckin := func(ctx context.Context) (kind string, credit int64, message string, err error) {
		if codeArtsProvider == nil {
			return string(codeartspkg.ClaimFailed), 0, "codearts 未接线", nil
		}
		started := time.Now()
		res, cerr := codeArtsProvider.Client().ClaimDailyCheckin(ctx)

		// 记入签到历史：面板「签到记录」此前只有 workbuddy，codearts 明明签了却看不见。
		status := "fail"
		switch string(res.Kind) {
		case "claimed":
			status = "ok"
		case "already-claimed":
			status = "already"
		}
		// UID 与 scheduler 的写法保持一致（codearts:<账号名>），
		// 否则同一个平台会出现两种 uid 形态，统计与前端匹配都要写两套。
		caName := "codearts"
		if cred, cerr := codeArtsProvider.Client().Credential(); cerr == nil && cred.Name != "" {
			caName = cred.Name
		}
		one := server.CheckinResult{
			UID: "codearts:" + caName, Nickname: caName,
			Provider: "codearts", Status: status,
		}
		if res.Credit > 0 {
			c := res.Credit
			one.Credits = &c
		}
		if cerr != nil {
			one.Detail = cerr.Error()
		} else if message == "" && res.Message != "" {
			one.Detail = res.Message
		}
		rep := server.CheckinReport{Enabled: true, Total: 1, Results: []server.CheckinResult{one}}
		switch status {
		case "ok":
			rep.OK = 1
		case "already":
			rep.Already = 1
		default:
			rep.Fail = 1
		}
		checkinStore.Append(rep, started, time.Now())

		if cerr != nil {
			return string(codeartspkg.ClaimFailed), 0, cerr.Error(), cerr
		}
		return string(res.Kind), res.Credit, res.Message, nil
	}

	sch := scheduler.New(scheduler.Config{
		Pool:                p,
		Upstream:            up,
		CheckinHours:        cfg.Schedule.CheckinHours,
		TravelHours:         cfg.Schedule.TravelHours,
		ActivityHours:       cfg.Schedule.ActivityHours,
		KeepaliveHours:      cfg.Schedule.KeepaliveHours,
		SchoolHours:         cfg.Schedule.SchoolHours,
		CatHours:            cfg.Schedule.CatHours,
		ActivityReportCount: cfg.Schedule.ActivityReportCount,
		// 触发时刻抖动窗口（schedule.jitter_minutes，0 = 精确整点 = 旧行为）。
		JitterMinutes: cfg.Schedule.JitterMinutes,
		// 实例盐（schedule.jitter_salt，缺省空 = 与引入前逐字一致）。
		JitterSalt: cfg.Schedule.JitterSalt,
		// 账号间随机抖动 + 档位小时摊开（schedule.account_delay_* / random_delay_minutes）。
		AccountDelayMinSeconds: cfg.Schedule.AccountDelayMinSeconds,
		AccountDelayMaxSeconds: cfg.Schedule.AccountDelayMaxSeconds,
		RandomDelayMinutes:     cfg.Schedule.RandomDelayMinutes,
		RandomDelaySeed:        scheduleSeed,
		ExpiringSoonWindow:     cfg.ExpiringSoonDur, // 快过期积分优先消耗（issue:积分过期）
		// CodeArts 签到：作为额外签到源接在 workbuddy 之后，独立失败不影响其他家。
		// 闭包捕获变量而非值：codeArtsProvider 就在上面构造，这里读的是同一个指针。
		ExtraCheckins: []func(context.Context) ([]scheduler.CheckinOutcome, error){
			func(ctx context.Context) ([]scheduler.CheckinOutcome, error) {
				if codeArtsProvider == nil {
					return nil, nil
				}
				name := codeArtsProvider.AccountName()
				if name == "" {
					name = "unknown"
				}
				kind, credit, msg, cerr := codeArtsCheckin(ctx)
				oc := scheduler.CheckinOutcome{UID: "codearts:" + name, Nickname: name}
				switch kind {
				case string(codeartspkg.ClaimClaimed):
					oc.Status = scheduler.CheckinOK
					if credit > 0 {
						c := credit
						oc.Credits = &c
					}
				case string(codeartspkg.ClaimAlready):
					oc.Status = scheduler.CheckinAlready
				case string(codeartspkg.ClaimInactive):
					oc.Status = scheduler.CheckinSkipped
					oc.Detail = msg
				default:
					oc.Status = scheduler.CheckinFail
					oc.Detail = msg
					if cerr != nil {
						oc.Detail = "签到失败: " + cerr.Error()
					}
				}
				return []scheduler.CheckinOutcome{oc}, nil
			},
		},
		// 任务执行台账与当日失败重试（schedule.ledger_file / retry_*）。
		// 重试默认关闭（RetryDelayMinutes=0），台账恒在（纯内存，除非给了落盘路径）。
		LedgerFile:        cfg.Schedule.LedgerFile,
		RetryDelayMinutes: cfg.Schedule.RetryDelayMinutes,
		RetryMaxPerDay:    cfg.Schedule.RetryMaxPerDay,
		CheckinDisabled:   !cfg.Schedule.CheckinEnabled,
		TravelDisabled:    !cfg.Schedule.TravelEnabled,
		ActivityDisabled:  !cfg.Schedule.ActivityEnabled,
		KeepaliveDisabled: !cfg.Schedule.KeepaliveEnabled,
		SchoolDisabled:    !cfg.Schedule.SchoolEnabled,
		CatDisabled:       !cfg.Schedule.CatEnabled,
		// 签到结束即落盘：手动入口与定时排程都走 CheckinAll，挂钩一处两条路径全覆盖。
		// 映射成 HTTP 口径复用 buildCheckinReport —— 保证「即时回执」与「历史回放」
		// 永远是同一份数据，不会出现两套计数口径。
		OnCheckinDone: func(outcomes []scheduler.CheckinOutcome, started, finished time.Time) {
			checkinStore.Append(
				buildCheckinReport(outcomes, realmOfFn(p), cfg.Schedule.CheckinEnabled, cfg.Schedule.CheckinHours),
				started, finished)
		},
	})
	switch {
	case !cfg.Schedule.CheckinEnabled:
		log.Printf("签到已禁用（schedule.checkin_enabled=false）")
	default:
		log.Printf("签到已启用：%v 点（签到 + 余额查询解冻）", cfg.Schedule.CheckinHours)
	}
	switch {
	case !cfg.Schedule.TravelEnabled:
		log.Printf("猫猫旅行已禁用（schedule.travel_enabled=false）")
	default:
		log.Printf("猫猫旅行已启用：%v 点（独立排程：领养 / 派出 / 领奖）", cfg.Schedule.TravelHours)
	}
	switch {
	case !cfg.Schedule.ActivityEnabled:
		log.Printf("活跃上报已禁用（schedule.activity_enabled=false）")
	default:
		log.Printf("活跃上报已启用：%v 点（每号 %d 条，点亮连登 + 补满领猫对话门槛）", cfg.Schedule.ActivityHours, cfg.Schedule.ActivityReportCount)
	}
	if !cfg.Schedule.KeepaliveEnabled {
		log.Printf("token 保活已禁用（schedule.keepalive_enabled=false）")
	} else {
		log.Printf("token 保活已启用：%v 点", cfg.Schedule.KeepaliveHours)
	}
	if !cfg.Schedule.SchoolEnabled {
		log.Printf("开学季任务已禁用（schedule.school_enabled=false）")
	} else {
		log.Printf("开学季任务已启用：%v 点（school_open_day_2026.py ALL --run --yes）", cfg.Schedule.SchoolHours)
	}
	if !cfg.Schedule.CatEnabled {
		log.Printf("夜猫子任务已禁用（schedule.cat_enabled=false）")
	} else {
		log.Printf("夜猫子任务已启用：%v 点（task_runner.py ALL --yes --only black_cat）", cfg.Schedule.CatHours)
	}
	if cfg.Schedule.JitterMinutes > 0 {
		log.Printf("排程抖动已启用：各任务触发时刻在名义整点后 0-%d 分钟内确定性偏移（schedule.jitter_minutes）",
			cfg.Schedule.JitterMinutes)
	}
	// 任务执行台账与当日失败重试（schedule.ledger_file / retry_*）。
	// 台账恒在（内存），落盘与否取决于 ledger_file；重试默认关闭。
	if cfg.Schedule.LedgerFile != "" {
		log.Printf("任务执行台账已落盘：%s（每类任务一轮执行后覆写，重启后仍可对账；实时视图见 /status 的 task_ledger）",
			cfg.Schedule.LedgerFile)
	} else {
		log.Printf("任务执行台账仅在内存（schedule.ledger_file 为空，重启后只剩新跑过的记录）")
	}
	if cfg.Schedule.RetryDelayMinutes > 0 && cfg.Schedule.RetryMaxPerDay > 0 {
		log.Printf("当日失败重试已启用：某类任务一轮「全灭」（有失败且无任何账号做成）后 %d 分钟补跑，每类每日最多 %d 次（schedule.retry_delay_minutes / retry_max_per_day）",
			cfg.Schedule.RetryDelayMinutes, cfg.Schedule.RetryMaxPerDay)
	} else {
		log.Printf("当日失败重试已关闭（schedule.retry_delay_minutes=%d retry_max_per_day=%d；全灭只记 WARN 与台账，不自动补跑）",
			cfg.Schedule.RetryDelayMinutes, cfg.Schedule.RetryMaxPerDay)
	}

	// 管理操作审计（config admin.audit_enabled，默认关闭）：把 /admin 下每个动作
	// 追加一行 JSONL 到磁盘。构造期做可写性预检并 fail-fast——路径不可写是审计最
	// 常见的失效原因，且完全能在启动时发现；放到第一次管理操作才暴露，等于把一次
	// 「配置错」推迟成「真出事时才发现审计是空的」，那正是审计最没用的时刻。
	var auditLog *server.AuditLog
	if cfg.Admin.AuditEnabled {
		al, err := server.NewAuditLog(cfg.Admin.AuditFile, cfg.APIKey)
		if err != nil {
			log.Fatalf("管理操作审计初始化失败：%v", err)
		}
		auditLog = al
		log.Printf("管理操作审计已启用：%s（每个 /admin 动作追加一行 JSONL）", al.Path())
	}

	// 当日积分预算闸（budget.daily_credit_limit，默认关闭）。
	if cfg.Budget.DailyCreditLimit > 0 {
		log.Printf("当日积分预算已启用：累计扣费达 %.2f credit 后拒服务（按 CST 自然日重置，实时用量见 /status 的 daily_budget）",
			cfg.Budget.DailyCreditLimit)
	}

	// ===== 大统一：进程内加载 doubao provider（不另起端口、不另起进程）=====
	var doubaoChat func(http.ResponseWriter, *http.Request)
	var doubaoHealth func() (healthy, total int)
	var doubaoAdmin http.Handler
	if cfg.Providers.Doubao.Enabled {
		if cfg.Providers.Doubao.Config == "" {
			log.Fatalf("providers.doubao.enabled=true 但未提供 providers.doubao.config")
		}
		dbCfg, err := dbconfig.Load(cfg.Providers.Doubao.Config)
		if err != nil {
			log.Fatalf("加载 doubao provider 配置失败: %v", err)
		}
		if acc := cfg.Providers.Doubao.Accounts; acc != "" {
			if err := os.Setenv("DOUBAO2API_ACCOUNTS", acc); err != nil {
				log.Fatalf("设置 doubao 账号路径失败: %v", err)
			}
		}
		dbSrv := dbserver.New(dbCfg)
		doubaoChat = dbSrv.HandleChat
		doubaoHealth = dbSrv.HealthStats
		doubaoAdmin = dbSrv
		dh, dt := dbSrv.HealthStats()
		log.Printf("doubao provider 已加载（config=%s, healthy=%d/%d）", cfg.Providers.Doubao.Config, dh, dt)
	}

	// ===== Trae provider：外部反代（Trae2api-cn 独立进程）=====
	// 与 doubao 的差别：doubao 是进程内 handler，Trae 是 HTTP 转发的独立服务。
	// 网关只负责把它纳入统一别名与故障转移，账号/OAuth/签到仍由 Trae2api 管。
	var traeChat func(http.ResponseWriter, *http.Request)
	var traeAdmin *server.TraeClient
	if cfg.Providers.Trae.Enabled {
		traeAdmin = server.NewTraeClient(cfg.Providers.Trae.BaseURL, cfg.Providers.Trae.APIKey)
		traeChat = traeAdmin.Chat
		log.Printf("trae provider 已接线（base=%s）", traeAdmin.BaseURL())
	}

	// Loomy：进程内 provider。必须定义在别名校验之前——校验会检查 loomy 候选。
	// 账号缺失不阻止启动：请求时回 503 并提示先在面板导入。
	loomyProvider := server.NewLoomyProvider("loomy/conversations.json")
	log.Printf("loomy provider 已接线（会话存储 loomy/conversations.json，已有 %d 条）",
		loomyProvider.Conversations().Len())

	// codeArtsChat 为 nil 时 unified 分支回 503 并提示未接线。
	var codeArtsChat func(http.ResponseWriter, *http.Request)
	if codeArtsProvider != nil {
		codeArtsChat = codeArtsProvider.Chat
	}

	// 别名启动期校验：拼错的 provider / 空 model 在启动时就暴露，不留到运行期。
	// 多候选别名（providers 数组）逐个校验；无候选视为配置错误。
	for name, a := range cfg.Aliases {
		cands := a.Candidates()
		if len(cands) == 0 {
			log.Fatalf("别名 %q 未配置 provider（provider 或 providers 均为空）", name)
		}
		for _, p := range cands {
			switch p {
			case "workbuddy":
			case "doubao":
				if doubaoChat == nil {
					log.Fatalf("别名 %q 指向 doubao，但 providers.doubao.enabled 未开启", name)
				}
			case "trae":
				if traeChat == nil {
					log.Fatalf("别名 %q 指向 trae，但 providers.trae.enabled 未开启", name)
				}
			case "loomy":
				// loomy 是进程内 provider，凭证缺失不阻止启动（请求时回 503 并提示导入）。
				if loomyProvider == nil {
					log.Fatalf("别名 %q 指向 loomy，但 loomy provider 未初始化", name)
				}
			case "codearts":
				// 同上：凭据缺失/过期不阻止启动，请求时回 503 并说明缺什么。
				if codeArtsProvider == nil {
					log.Fatalf("别名 %q 指向 codearts，但 providers.codearts.enabled 未开启", name)
				}
			default:
				log.Fatalf("别名 %q 的 provider %q 未知（仅支持 workbuddy / doubao / trae / loomy / codearts）", name, p)
			}
		}
		if a.Model == "" {
			log.Fatalf("别名 %q 未配置上游 model", name)
		}
	}
	if len(cfg.Aliases) > 0 {
		log.Printf("全局别名已启用：%d 条（只暴露别名，未注册别名返回 400）", len(cfg.Aliases))
	}

	h := server.NewHandler(server.Config{
		Pool:         p,
		Upstream:     up,
		APIKey:       cfg.APIKey,
		AuthKeys:     buildAuthKeys(cfg),
		Session:      sessRouter,
		StickyCount:  sessCount,
		RedisMode:    redisMode,
		SoftCooldown: cfg.SoftRateDur,
		PromptMode:   cfg.Prompt.Mode,
		PromptText:   cfg.PromptText,
		// global realm 开关（handler 侧第三道闸：modelList 据此决定是否列 global 名单）。
		GlobalEnabled: cfg.Global.Enabled,
		// 运维管理端点开关（config admin.enabled，默认 false）。
		AdminEnabled: cfg.Admin.Enabled,
		// Prometheus 指标端点开关（config metrics.enabled，默认 false）。
		MetricsEnabled: cfg.Metrics.Enabled,
		// 手动任务触发（admin.enabled 下的 /admin/tasks/{name}/run）：把调度器
		// 作为 TaskRunner 注入，server 包不必反向 import scheduler。
		Tasks: sch,
		// 管理操作审计接收器（admin.audit_enabled，默认关闭时为零值 nil =
		// 不审计、零开销）。
		Audit: auditLog,
		// 当日积分预算上限（budget.daily_credit_limit，0 = 关闭该闸）。
		BudgetLimit: cfg.Budget.DailyCreditLimit,
		// 任务执行台账只读视图（/status 的 task_ledger 段 + /metrics 的任务指标）。
		// 与 Tasks 同款：*taskledger.Store 结构上即满足 server 侧的窄接口，
		// server 包不必反向 import scheduler。
		TaskLedger: sch.Ledger(),
		// 手动签到入口（POST /v1/checkin）。★ 走进程内 CheckinAll ★ 外部 CLI
		// 签到不会更新网关内存额度（见 internal/server/checkin.go 顶部注释）。
		CheckinFn: checkinReportFn(sch, p, cfg),
		// 签到历史（GET /v1/checkin/history）与排程快照（下一个自动签到时点，
		// 含 jitter 派生）。*scheduler.Scheduler 同时实现 CheckinScheduleProvider。
		CheckinHistory:  checkinStore,
		CheckinSchedule: sch,
		// 大统一：全局别名与进程内 provider 入口。
		Aliases:      cfg.Aliases,
		DoubaoChat:   doubaoChat,
		DoubaoHealth: doubaoHealth,
		DoubaoAdmin:  doubaoAdmin,
		// Trae：外部反代，nil 时别名里的 trae 候选会被跳过并报 503。
		TraeChat:  traeChat,
		TraeAdmin: traeAdmin,
		// Loomy：进程内 provider，凭证来自 loomy/accounts.json。
		LoomyChat:       loomyProvider.Chat,
		CodeArtsChat:    codeArtsChat,
		CodeArtsCheckin: codeArtsCheckin,
		// Web 登录的回调端口：用网关自己的端口，授权页的 port 参数必须与之一致。
		TraeBaseURL:       cfg.Providers.Trae.BaseURL,
		CodeArtsLoginPort: codeArtsLoginPort(cfg.Listen),
		CodeArtsExchange: func(ctx context.Context, code, verifier string, port int, jwk *codeartspkg.DpopJWK) (string, string, error) {
			if codeArtsProvider == nil {
				return "", "", errors.New("codearts 未接线")
			}
			cred, err := codeArtsProvider.Client().ExchangeCode(ctx, code, verifier, port, jwk)
			if err != nil {
				return "", "", err
			}
			if err := codeartspkg.SaveCredential(codeArtsCredPath, cred); err != nil {
				return "", "", err
			}
			return cred.Name, cred.ExpiresAt, nil
		},
		CodeArtsImport: func(ctx context.Context) (string, string, error) {
			if codeArtsProvider == nil {
				return "", "", errors.New("codearts 未接线")
			}
			cli := codeArtsProvider.Client()
			cur, _ := cli.Credential()
			ide, err := codeartspkg.ImportFromIDE()
			if err != nil {
				return "", "", err
			}
			// 接管模式下 IDE 那份可能已经失效：只接受**比当前更新**的凭据，
			// 避免误点「导入」把能用的换成一具尸体。
			if cur.ExpiresAt != "" && ide.ExpiresAt != "" && ide.ExpiresAt <= cur.ExpiresAt {
				return "", "", errors.New("IDE 里的凭据不比当前新，已拒绝覆盖（接管模式下 IDE 可能已失效）")
			}
			if err := codeartspkg.SaveCredential(codeArtsCredPath, ide); err != nil {
				return "", "", err
			}
			cred, _ := cli.Credential()
			return cred.Name, cred.ExpiresAt, nil
		},
		CodeArtsHealth: func() (int, int) {
			if codeArtsProvider == nil {
				return 0, 0
			}
			cred, cerr := codeArtsProvider.Client().Credential()
			if cerr != nil || !cred.HasSTS() {
				return 0, 1
			}
			if cred.ExpiresAt != "" {
				if t, perr := time.Parse(time.RFC3339, cred.ExpiresAt); perr == nil && time.Now().After(t) {
					return 0, 1
				}
			}
			return 1, 1
		},
		CodeArtsPanel: func(ctx context.Context) server.CodeArtsPanelInfo {
			if codeArtsProvider == nil {
				return server.CodeArtsPanelInfo{Err: errors.New("codearts 未接线")}
			}
			info := server.CodeArtsPanelInfo{Name: codeArtsProvider.AccountName()}
			if cred, cerr := codeArtsProvider.Client().Credential(); cerr == nil {
				info.ExpiresAt = cred.ExpiresAt
			}
			info.Quota, info.Err = codeArtsProvider.Client().Quota(ctx)
			return info
		},
		PanelEnabled: cfg.Panel.Enabled,
		PanelAuth:    cfg.Panel.Auth,
		// 面板账号新增所需路径：凭证目录、OAuth 登录程序、豆包账号文件。
		// login.exe 必须给绝对路径：Go exec.Command 在 Windows 下对相对路径的
		// "当前目录解析"有安全限制（ErrDot），实测 "login.exe" 直接 exec 失败。
		AuthDir: cfg.AuthDir,
		LoginBin: func() string {
			// 与主程序同目录；扩展名按平台取，否则 Linux/macOS/Docker 里
			// 编译出的 `login` 永远不会被找到（面板 OAuth 报 oauth_unavailable）。
			name := "login"
			if runtime.GOOS == "windows" {
				name = "login.exe"
			}
			exe, err := os.Executable()
			if err != nil {
				return name
			}
			return filepath.Join(filepath.Dir(exe), name)
		}(),
		DoubaoAccountsFile: cfg.Providers.Doubao.Accounts,
		// Provider 路由优先序落盘：数据目录下独立小文件（与 state.json 同目录）。
		// 空 StateFile 时留空 = 只在本进程生效。
		ProviderOrderFile: providerOrderJSONPath(cfg.StateFile),
	})
	// trae 的上下文能力来自它自己的 /v1/models（2026-11-22 起带 context_window）。
	if cfg.Providers.Trae.Enabled {
		loadTraeCapabilities(cfg.Providers.Trae.BaseURL)
	}

	// 配置热载：别名改完不用重启（listen/providers/api_key 仍要求重启）。
	watchConfig(h, *cfgPath, *cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	// 可用性阈值告警（config alerting.enabled，默认关闭）：独立 ticker 评估只读健康
	// 快照，越界时 POST 到运维自备的 webhook。与请求路径完全隔离，且从不调用上游。
	alertMon := alert.New(alert.Config{
		Enabled:          cfg.Alerting.Enabled,
		WebhookURL:       cfg.Alerting.WebhookURL,
		Secret:           cfg.Alerting.Secret,
		Interval:         time.Duration(cfg.Alerting.IntervalSeconds) * time.Second,
		Timeout:          time.Duration(cfg.Alerting.TimeoutSeconds) * time.Second,
		StartupGrace:     time.Duration(cfg.Alerting.StartupGraceSeconds) * time.Second,
		MinHealthyCN:     cfg.Alerting.MinHealthyCN,
		MinHealthyGlobal: cfg.Alerting.MinHealthyGlobal,
		RecoverHealthy:   cfg.Alerting.RecoverHealthy,
		BreakerThreshold: cfg.Alerting.BreakerThreshold,
		ForTicks:         cfg.Alerting.ForTicks,
		ClearTicks:       cfg.Alerting.ClearTicks,
		SendResolve:      cfg.Alerting.SendResolve,
		ServiceName:      server.ServiceName,
	}, alertSource{pool: p, h: h})
	go alertMon.Run(ctx)
	defer alertMon.Stop()
	if !cfg.Alerting.Enabled {
		log.Printf("可用性告警已禁用（alerting.enabled=false）")
	} else {
		log.Printf("可用性告警已启用：每 %ds 评估，健康阈值 cn=%d / global=%d（0=关闭该规则），熔断阈值=%d（0=关闭）",
			cfg.Alerting.IntervalSeconds, cfg.Alerting.MinHealthyCN,
			cfg.Alerting.MinHealthyGlobal, cfg.Alerting.BreakerThreshold)
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		// ReadTimeout 覆盖整个请求读取（含 body）：防慢速 body 拖死连接。
		// max_body_mb 已移除（请求体无上限，交由上游自然响应），超大 body 成为
		// 唯一的自然约束：60s 内传不完会得到连接错误（read timeout）而非 413。
		ReadTimeout: 60 * time.Second,
		// IdleTimeout keep-alive 空闲连接回收：配合 ctx 传播（FIX-2）防连接泄漏堆积。
		// 注意：SSE 流式响应期间连接非空闲，不受此项掐断；不设全局 WriteTimeout
		// （长流式生成合法时长可达数分钟，全局 WriteTimeout 会误杀在途 SSE）。
		IdleTimeout: 120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		p.Flush() // 信号触发：先落盘再做优雅停机
		// Flush 已把最后一笔状态快照提交给 Redis（fire-and-forget）；store.Close
		// 等 Upstash 在途/排队写排空再关连接——最后一笔镜像必须写完才退出（发现 4）。
		// Noop 的 Close 是空操作；单写上限 5s × 上限 8，Close 内部另有超时兜底。
		if cErr := store.Close(); cErr != nil {
			log.Printf("WARN: [server] redisstore close: %v", cErr)
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	if cfg.Global.Enabled {
		log.Printf("global realm 已启用（chat_base=%q billing_base=%q，空=默认 workbuddy.ai）",
			cfg.Global.ChatBase, cfg.Global.BillingBase)
	} else {
		log.Printf("global realm 已禁用（config global.enabled=false，纯 CN）")
	}
	// 落盘路径清单：备份/迁移/排查时先看这两行，不用在多个目录里猜。
	// 路径有一部分由各 provider 内部决定，这里只**展示**，不改变行为。
	{
		dir := filepath.Dir(cfg.StateFile)
		log.Printf("落盘路径：config=%s | state=%s | model=%s | checkin=%s | provorder=%s | usage=%s",
			*cfgPath, cfg.StateFile,
			filepath.Join(dir, "model.json"),
			filepath.Join(dir, "checkin.json"),
			filepath.Join(dir, "provider_order.json"),
			filepath.Join(dir, "usage.json"))
		log.Printf("凭据路径：keys=keys.json | codearts=codearts/accounts.json | loomy=loomy/accounts.json | trae=trae/data/accounts.json | doubao=doubao2api/config.json")
		// 权限自检：非 Windows 上「同机其他用户可读」是真问题；Windows 只跳过。
		checkSecretPerms([]string{*cfgPath, "keys.json", "codearts/accounts.json", "loomy/accounts.json", "trae/data/accounts.json"})
	}
	log.Printf("unified-api-v2 listening on %s (api_key=%v, aliases=%d)", cfg.Listen, cfg.APIKey != "", len(cfg.Aliases))
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// main 是两种运行模式的入口：
//
//	unified-api-v2                    前台运行（开发/手工调试）
//	unified-api-v2 install-service    注册为 Windows 服务（失败自动重启）
//	unified-api-v2 uninstall-service  卸载服务
//	（无参数且由 SCM 启动）            以服务方式运行
//
// 服务模式下必须先把工作目录切到 exe 所在目录：SCM 默认工作目录是
// C:/Windows/System32，直接启动会找不到 config.json。
func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "install-service":
			if err := installService(); err != nil {
				log.Fatalf("安装服务失败: %v", err)
			}
			fmt.Println("服务已安装: " + serviceName + "（已配置失败自动重启）")
			return
		case "uninstall-service":
			if err := uninstallService(); err != nil {
				log.Fatalf("卸载服务失败: %v", err)
			}
			fmt.Println("服务已卸载: " + serviceName)
			return
		case "start-service":
			if err := startService(); err != nil {
				log.Fatalf("启动服务失败: %v", err)
			}
			fmt.Println("服务已启动")
			return
		case "stop-service":
			if err := stopService(); err != nil {
				log.Fatalf("停止服务失败: %v", err)
			}
			fmt.Println("服务已停止")
			return
		}
	}
	if isWindowsService() {
		if exe, err := os.Executable(); err == nil {
			_ = os.Chdir(filepath.Dir(exe))
		}
		redirectServiceLogs()
		runAsService()
		return
	}
	// -detach：由计划任务/看护拉起时使用，启动即脱离控制台，不留黑窗口。
	// 日志改为追加到 gateway.err.log（stdout 已不可用）。
	for _, a := range os.Args[1:] {
		if a == "-detach" || a == "--detach" {
			if exe, err := os.Executable(); err == nil {
				_ = os.Chdir(filepath.Dir(exe))
			}
			redirectServiceLogs()
			detachConsole()
			break
		}
	}
	runServer()
}
