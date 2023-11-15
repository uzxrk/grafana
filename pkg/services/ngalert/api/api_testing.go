package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/benbjohnson/clock"
	"github.com/grafana/alerting/models"
	amv2 "github.com/prometheus/alertmanager/api/v2/models"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/grafana/grafana/pkg/api/response"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/tracing"
	contextmodel "github.com/grafana/grafana/pkg/services/contexthandler/model"
	"github.com/grafana/grafana/pkg/services/datasources"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/ngalert/accesscontrol"
	apimodels "github.com/grafana/grafana/pkg/services/ngalert/api/tooling/definitions"
	"github.com/grafana/grafana/pkg/services/ngalert/backtesting"
	"github.com/grafana/grafana/pkg/services/ngalert/eval"
	ngmodels "github.com/grafana/grafana/pkg/services/ngalert/models"
	"github.com/grafana/grafana/pkg/services/ngalert/state"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
)

type TestingApiSrv struct {
	*AlertingProxy
	DatasourceCache datasources.CacheService
	log             log.Logger
	authz           RuleAccessControlService
	evaluator       eval.EvaluatorFactory
	cfg             *setting.UnifiedAlertingSettings
	backtesting     *backtesting.Engine
	featureManager  featuremgmt.FeatureToggles
	appUrl          *url.URL
	tracer          tracing.Tracer
}

// RouteTestGrafanaRuleConfig returns a list of potential alerts for a given rule configuration. This is intended to be
// as true as possible to what would be generated by the ruler except that the resulting alerts are not filtered to
// only Resolved / Firing and ready to send.
func (srv TestingApiSrv) RouteTestGrafanaRuleConfig(c *contextmodel.ReqContext, body apimodels.PostableExtendedRuleNodeExtended) response.Response {
	rule, err := validateRuleNode(
		&body.Rule,
		body.RuleGroup,
		srv.cfg.BaseInterval,
		c.SignedInUser.GetOrgID(),
		&folder.Folder{
			OrgID: c.SignedInUser.GetOrgID(),
			UID:   body.NamespaceUID,
			Title: body.NamespaceTitle,
		},
		srv.cfg,
	)
	if err != nil {
		return ErrResp(http.StatusBadRequest, err, "")
	}

	if !srv.authz.AuthorizeAccessToRuleGroup(c.Req.Context(), c.SignedInUser, ngmodels.RulesGroup{rule}) {
		return errorToResponse(fmt.Errorf("%w to query one or many data sources used by the rule", accesscontrol.ErrAuthorization))
	}

	evaluator, err := srv.evaluator.Create(eval.NewContext(c.Req.Context(), c.SignedInUser), rule.GetEvalCondition())
	if err != nil {
		return ErrResp(http.StatusBadRequest, err, "Failed to build evaluator for queries and expressions")
	}

	now := time.Now()
	results, err := evaluator.Evaluate(c.Req.Context(), now)
	if err != nil {
		return ErrResp(http.StatusInternalServerError, err, "Failed to evaluate queries")
	}

	cfg := state.ManagerCfg{
		Metrics:                 nil,
		ExternalURL:             srv.appUrl,
		InstanceStore:           nil,
		Images:                  &backtesting.NoopImageService{},
		Clock:                   clock.New(),
		Historian:               nil,
		MaxStateSaveConcurrency: 1,
		Tracer:                  srv.tracer,
		Log:                     log.New("ngalert.state.manager"),
	}
	manager := state.NewManager(cfg)
	includeFolder := !srv.cfg.ReservedLabels.IsReservedLabelDisabled(models.FolderTitleLabel)
	transitions := manager.ProcessEvalResults(
		c.Req.Context(),
		now,
		rule,
		results,
		state.GetRuleExtraLabels(rule, body.NamespaceTitle, includeFolder),
	)

	alerts := make([]*amv2.PostableAlert, 0, len(transitions))
	for _, alertState := range transitions {
		alerts = append(alerts, state.StateToPostableAlert(alertState.State, srv.appUrl))
	}

	return response.JSON(http.StatusOK, alerts)
}

func (srv TestingApiSrv) RouteTestRuleConfig(c *contextmodel.ReqContext, body apimodels.TestRulePayload, datasourceUID string) response.Response {
	if body.Type() != apimodels.LoTexRulerBackend {
		return errorToResponse(backendTypeDoesNotMatchPayloadTypeError(apimodels.LoTexRulerBackend, body.Type().String()))
	}
	ds, err := getDatasourceByUID(c, srv.DatasourceCache, apimodels.LoTexRulerBackend)
	if err != nil {
		return errorToResponse(err)
	}
	var path string
	switch ds.Type {
	case "loki":
		path = "loki/api/v1/query"
	case "prometheus":
		path = "api/v1/query"
	default:
		// this should not happen because getDatasourceByUID would not return the data source
		return errorToResponse(unexpectedDatasourceTypeError(ds.Type, "loki, prometheus"))
	}

	t := timeNow()
	queryURL, err := url.Parse(path)
	if err != nil {
		return ErrResp(http.StatusInternalServerError, err, "failed to parse url")
	}
	params := queryURL.Query()
	params.Set("query", body.Expr)
	params.Set("time", strconv.FormatInt(t.Unix(), 10))
	queryURL.RawQuery = params.Encode()
	return srv.withReq(
		c,
		http.MethodGet,
		queryURL,
		nil,
		instantQueryResultsExtractor,
		nil,
	)
}

func (srv TestingApiSrv) RouteEvalQueries(c *contextmodel.ReqContext, cmd apimodels.EvalQueriesPayload) response.Response {
	queries := AlertQueriesFromApiAlertQueries(cmd.Data)
	if !srv.authz.AuthorizeAccessToRuleGroup(c.Req.Context(), c.SignedInUser, ngmodels.RulesGroup{&ngmodels.AlertRule{Data: queries}}) {
		return ErrResp(http.StatusUnauthorized, fmt.Errorf("%w to query one or many data sources used by the rule", accesscontrol.ErrAuthorization), "")
	}

	cond := ngmodels.Condition{
		Condition: "",
		Data:      queries,
	}
	if len(cmd.Data) > 0 {
		cond.Condition = cmd.Data[0].RefID
	}
	evaluator, err := srv.evaluator.Create(eval.NewContext(c.Req.Context(), c.SignedInUser), cond)

	if err != nil {
		return ErrResp(http.StatusBadRequest, err, "Failed to build evaluator for queries and expressions")
	}

	now := cmd.Now
	if now.IsZero() {
		now = timeNow()
	}

	evalResults, err := evaluator.EvaluateRaw(c.Req.Context(), now)

	if err != nil {
		return ErrResp(http.StatusInternalServerError, err, "Failed to evaluate queries and expressions")
	}

	return response.JSONStreaming(http.StatusOK, evalResults)
}

func (srv TestingApiSrv) BacktestAlertRule(c *contextmodel.ReqContext, cmd apimodels.BacktestConfig) response.Response {
	if !srv.featureManager.IsEnabled(c.Req.Context(), featuremgmt.FlagAlertingBacktesting) {
		return ErrResp(http.StatusNotFound, nil, "Backgtesting API is not enabled")
	}

	if cmd.From.After(cmd.To) {
		return ErrResp(400, nil, "From cannot be greater than To")
	}

	noDataState, err := ngmodels.NoDataStateFromString(string(cmd.NoDataState))

	if err != nil {
		return ErrResp(400, err, "")
	}
	forInterval := time.Duration(cmd.For)
	if forInterval < 0 {
		return ErrResp(400, nil, "Bad For interval")
	}

	intervalSeconds, err := validateInterval(srv.cfg, time.Duration(cmd.Interval))
	if err != nil {
		return ErrResp(400, err, "")
	}

	queries := AlertQueriesFromApiAlertQueries(cmd.Data)
	if !srv.authz.AuthorizeAccessToRuleGroup(c.Req.Context(), c.SignedInUser, ngmodels.RulesGroup{&ngmodels.AlertRule{Data: queries}}) {
		return errorToResponse(fmt.Errorf("%w to query one or many data sources used by the rule", accesscontrol.ErrAuthorization))
	}

	rule := &ngmodels.AlertRule{
		// ID:             0,
		// Updated:        time.Time{},
		// Version:        0,
		// NamespaceUID:   "",
		// DashboardUID:   nil,
		// PanelID:        nil,
		// RuleGroup:      "",
		// RuleGroupIndex: 0,
		// ExecErrState:   "",
		Title: cmd.Title,
		// prefix backtesting- is to distinguish between executions of regular rule and backtesting in logs (like expression engine, evaluator, state manager etc)
		UID:             "backtesting-" + util.GenerateShortUID(),
		OrgID:           c.SignedInUser.GetOrgID(),
		Condition:       cmd.Condition,
		Data:            queries,
		IntervalSeconds: intervalSeconds,
		NoDataState:     noDataState,
		For:             forInterval,
		Annotations:     cmd.Annotations,
		Labels:          cmd.Labels,
	}

	result, err := srv.backtesting.Test(c.Req.Context(), c.SignedInUser, rule, cmd.From, cmd.To)
	if err != nil {
		if errors.Is(err, backtesting.ErrInvalidInputData) {
			return ErrResp(400, err, "Failed to evaluate")
		}
		return ErrResp(500, err, "Failed to evaluate")
	}

	body, err := data.FrameToJSON(result, data.IncludeAll)
	if err != nil {
		return ErrResp(500, err, "Failed to convert frame to JSON")
	}
	return response.JSON(http.StatusOK, body)
}
