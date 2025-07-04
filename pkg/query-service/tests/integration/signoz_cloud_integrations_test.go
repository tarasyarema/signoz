package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/SigNoz/signoz/pkg/alertmanager"
	"github.com/SigNoz/signoz/pkg/alertmanager/alertmanagerserver"
	"github.com/SigNoz/signoz/pkg/alertmanager/signozalertmanager"
	"github.com/SigNoz/signoz/pkg/analytics/analyticstest"
	"github.com/SigNoz/signoz/pkg/emailing/emailingtest"
	"github.com/SigNoz/signoz/pkg/sharder"
	"github.com/SigNoz/signoz/pkg/sharder/noopsharder"
	"github.com/SigNoz/signoz/pkg/types/authtypes"

	"github.com/SigNoz/signoz/pkg/http/middleware"
	"github.com/SigNoz/signoz/pkg/modules/organization/implorganization"
	"github.com/SigNoz/signoz/pkg/modules/user"
	"github.com/SigNoz/signoz/pkg/signoz"

	"github.com/SigNoz/signoz/pkg/instrumentation/instrumentationtest"
	"github.com/SigNoz/signoz/pkg/query-service/app"
	"github.com/SigNoz/signoz/pkg/query-service/app/cloudintegrations"
	"github.com/SigNoz/signoz/pkg/query-service/utils"
	"github.com/SigNoz/signoz/pkg/sqlstore"
	"github.com/SigNoz/signoz/pkg/types"
	"github.com/google/uuid"
	mockhouse "github.com/srikanthccv/ClickHouse-go-mock"
	"github.com/stretchr/testify/require"
)

func TestAWSIntegrationAccountLifecycle(t *testing.T) {
	// Test for happy path of connecting and managing AWS integration accounts

	require := require.New(t)
	testbed := NewCloudIntegrationsTestBed(t, nil)

	accountsListResp := testbed.GetConnectedAccountsListFromQS("aws")
	require.Equal(len(accountsListResp.Accounts), 0,
		"No accounts should be connected at the beginning",
	)

	// Should be able to generate a connection url from UI - initializing an integration account
	testAccountConfig := types.AccountConfig{
		EnabledRegions: []string{"us-east-1", "us-east-2"},
	}
	connectionUrlResp := testbed.GenerateConnectionUrlFromQS(
		"aws", cloudintegrations.GenerateConnectionUrlRequest{
			AgentConfig: cloudintegrations.SigNozAgentConfig{
				Region: "us-east-1",
			},
			AccountConfig: testAccountConfig,
		})
	testAccountId := connectionUrlResp.AccountId
	require.NotEmpty(testAccountId)
	connectionUrl := connectionUrlResp.ConnectionUrl
	require.NotEmpty(connectionUrl)

	// Should be able to poll for account connection status from the UI
	accountStatusResp := testbed.GetAccountStatusFromQS("aws", testAccountId)
	require.Equal(testAccountId, accountStatusResp.Id)
	require.Nil(accountStatusResp.Status.Integration.LastHeartbeatTsMillis)
	require.Nil(accountStatusResp.CloudAccountId)

	// The unconnected account should not show up in connected accounts list yet
	accountsListResp1 := testbed.GetConnectedAccountsListFromQS("aws")
	require.Equal(0, len(accountsListResp1.Accounts))

	// An agent installed in user's AWS account should be able to check in for the new integration account
	tsMillisBeforeAgentCheckIn := time.Now().UnixMilli()
	testAWSAccountId := "4563215233"
	agentCheckInResp := testbed.CheckInAsAgentWithQS(
		"aws", cloudintegrations.AgentCheckInRequest{
			ID:        testAccountId,
			AccountID: testAWSAccountId,
		},
	)
	require.Equal(testAccountId, agentCheckInResp.AccountId)
	require.Equal(testAWSAccountId, agentCheckInResp.CloudAccountId)
	require.Nil(agentCheckInResp.RemovedAt)

	// Polling for connection status from UI should now return latest status
	accountStatusResp1 := testbed.GetAccountStatusFromQS("aws", testAccountId)
	require.Equal(testAccountId, accountStatusResp1.Id)
	require.NotNil(accountStatusResp1.CloudAccountId)
	require.Equal(testAWSAccountId, *accountStatusResp1.CloudAccountId)
	require.NotNil(accountStatusResp1.Status.Integration.LastHeartbeatTsMillis)
	require.LessOrEqual(
		tsMillisBeforeAgentCheckIn,
		*accountStatusResp1.Status.Integration.LastHeartbeatTsMillis,
	)

	// The account should now show up in list of connected accounts.
	accountsListResp2 := testbed.GetConnectedAccountsListFromQS("aws")
	require.Equal(len(accountsListResp2.Accounts), 1)
	require.Equal(testAccountId, accountsListResp2.Accounts[0].Id)
	require.Equal(testAWSAccountId, accountsListResp2.Accounts[0].CloudAccountId)

	// Should be able to update account config from UI
	testAccountConfig2 := types.AccountConfig{
		EnabledRegions: []string{"us-east-2", "us-west-1"},
	}
	latestAccount := testbed.UpdateAccountConfigWithQS(
		"aws", testAccountId, testAccountConfig2,
	)
	require.Equal(testAccountId, latestAccount.ID.StringValue())
	require.Equal(testAccountConfig2, *latestAccount.Config)

	// The agent should now receive latest account config.
	agentCheckInResp1 := testbed.CheckInAsAgentWithQS(
		"aws", cloudintegrations.AgentCheckInRequest{
			ID:        testAccountId,
			AccountID: testAWSAccountId,
		},
	)
	require.Equal(testAccountId, agentCheckInResp1.AccountId)
	require.Equal(testAWSAccountId, agentCheckInResp1.CloudAccountId)
	require.Nil(agentCheckInResp1.RemovedAt)

	// Should be able to disconnect/remove account from UI.
	tsBeforeDisconnect := time.Now()
	latestAccount = testbed.DisconnectAccountWithQS("aws", testAccountId)
	require.Equal(testAccountId, latestAccount.ID.StringValue())
	require.LessOrEqual(tsBeforeDisconnect, *latestAccount.RemovedAt)

	// The agent should receive the disconnected status in account config post disconnection
	agentCheckInResp2 := testbed.CheckInAsAgentWithQS(
		"aws", cloudintegrations.AgentCheckInRequest{
			ID:        testAccountId,
			AccountID: testAWSAccountId,
		},
	)
	require.Equal(testAccountId, agentCheckInResp2.AccountId)
	require.Equal(testAWSAccountId, agentCheckInResp2.CloudAccountId)
	require.LessOrEqual(tsBeforeDisconnect, *agentCheckInResp2.RemovedAt)
}

func TestAWSIntegrationServices(t *testing.T) {
	require := require.New(t)

	testbed := NewCloudIntegrationsTestBed(t, nil)

	// should be able to list available cloud services.
	svcListResp := testbed.GetServicesFromQS("aws", nil)
	require.Greater(len(svcListResp.Services), 0)
	for _, svc := range svcListResp.Services {
		require.NotEmpty(svc.Id)
		require.Nil(svc.Config)
	}

	// should be able to get details of a particular service.
	svcId := svcListResp.Services[0].Id
	svcDetailResp := testbed.GetServiceDetailFromQS("aws", svcId, nil)
	require.Equal(svcId, svcDetailResp.Id)
	require.NotEmpty(svcDetailResp.Overview)
	require.Nil(svcDetailResp.Config)
	require.Nil(svcDetailResp.ConnectionStatus)

	// should be able to configure a service in the ctx of a connected account

	// create a connected account
	testAccountId := uuid.NewString()
	testAWSAccountId := "389389489489"
	testbed.CheckInAsAgentWithQS(
		"aws", cloudintegrations.AgentCheckInRequest{
			ID:        testAccountId,
			AccountID: testAWSAccountId,
		},
	)

	testSvcConfig := types.CloudServiceConfig{
		Metrics: &types.CloudServiceMetricsConfig{
			Enabled: true,
		},
	}
	updateSvcConfigResp := testbed.UpdateServiceConfigWithQS("aws", svcId, cloudintegrations.UpdateServiceConfigRequest{
		CloudAccountId: testAWSAccountId,
		Config:         testSvcConfig,
	})
	require.Equal(svcId, updateSvcConfigResp.Id)
	require.Equal(testSvcConfig, updateSvcConfigResp.Config)

	// service list should include config when queried in the ctx of an account
	svcListResp = testbed.GetServicesFromQS("aws", &testAWSAccountId)
	require.Greater(len(svcListResp.Services), 0)
	for _, svc := range svcListResp.Services {
		if svc.Id == svcId {
			require.NotNil(svc.Config)
			require.Equal(testSvcConfig, *svc.Config)
		}
	}

	// service detail should include config and status info when
	// queried in the ctx of an account
	svcDetailResp = testbed.GetServiceDetailFromQS("aws", svcId, &testAWSAccountId)
	require.Equal(svcId, svcDetailResp.Id)
	require.NotNil(svcDetailResp.Config)
	require.Equal(testSvcConfig, *svcDetailResp.Config)

}

func TestConfigReturnedWhenAgentChecksIn(t *testing.T) {
	require := require.New(t)

	testbed := NewCloudIntegrationsTestBed(t, nil)

	// configure a connected account
	testAccountConfig := types.AccountConfig{
		EnabledRegions: []string{"us-east-1", "us-east-2"},
	}
	connectionUrlResp := testbed.GenerateConnectionUrlFromQS(
		"aws", cloudintegrations.GenerateConnectionUrlRequest{
			AgentConfig: cloudintegrations.SigNozAgentConfig{
				Region:       "us-east-1",
				SigNozAPIKey: "test-api-key",
			},
			AccountConfig: testAccountConfig,
		},
	)
	testAccountId := connectionUrlResp.AccountId
	require.NotEmpty(testAccountId)
	require.NotEmpty(connectionUrlResp.ConnectionUrl)

	testAWSAccountId := "389389489489"
	checkinResp := testbed.CheckInAsAgentWithQS(
		"aws", cloudintegrations.AgentCheckInRequest{
			ID:        testAccountId,
			AccountID: testAWSAccountId,
		},
	)

	require.Equal(testAccountId, checkinResp.AccountId)
	require.Equal(testAWSAccountId, checkinResp.CloudAccountId)
	require.Nil(checkinResp.RemovedAt)
	require.Equal(testAccountConfig.EnabledRegions, checkinResp.IntegrationConfig.EnabledRegions)

	telemetryCollectionStrategy := checkinResp.IntegrationConfig.TelemetryCollectionStrategy
	require.Equal("aws", telemetryCollectionStrategy.Provider)
	require.NotNil(telemetryCollectionStrategy.AWSMetrics)
	require.Empty(telemetryCollectionStrategy.AWSMetrics.StreamFilters)
	require.NotNil(telemetryCollectionStrategy.AWSLogs)
	require.Empty(telemetryCollectionStrategy.AWSLogs.Subscriptions)

	// helper
	setServiceConfig := func(svcId string, metricsEnabled bool, logsEnabled bool) {
		testSvcConfig := types.CloudServiceConfig{}
		if metricsEnabled {
			testSvcConfig.Metrics = &types.CloudServiceMetricsConfig{
				Enabled: metricsEnabled,
			}
		}
		if logsEnabled {
			testSvcConfig.Logs = &types.CloudServiceLogsConfig{
				Enabled: logsEnabled,
			}
		}

		updateSvcConfigResp := testbed.UpdateServiceConfigWithQS("aws", svcId, cloudintegrations.UpdateServiceConfigRequest{
			CloudAccountId: testAWSAccountId,
			Config:         testSvcConfig,
		})
		require.Equal(svcId, updateSvcConfigResp.Id)
		require.Equal(testSvcConfig, updateSvcConfigResp.Config)
	}

	setServiceConfig("ec2", true, false)
	setServiceConfig("rds", true, true)

	checkinResp = testbed.CheckInAsAgentWithQS(
		"aws", cloudintegrations.AgentCheckInRequest{
			ID:        testAccountId,
			AccountID: testAWSAccountId,
		},
	)

	require.Equal(testAccountId, checkinResp.AccountId)
	require.Equal(testAWSAccountId, checkinResp.CloudAccountId)
	require.Nil(checkinResp.RemovedAt)

	integrationConf := checkinResp.IntegrationConfig
	require.Equal(testAccountConfig.EnabledRegions, integrationConf.EnabledRegions)

	telemetryCollectionStrategy = integrationConf.TelemetryCollectionStrategy
	require.Equal("aws", telemetryCollectionStrategy.Provider)
	require.NotNil(telemetryCollectionStrategy.AWSMetrics)
	metricStreamNamespaces := []string{}
	for _, f := range telemetryCollectionStrategy.AWSMetrics.StreamFilters {
		metricStreamNamespaces = append(metricStreamNamespaces, f.Namespace)
	}
	require.Equal([]string{"AWS/EC2", "CWAgent", "AWS/RDS"}, metricStreamNamespaces)

	require.NotNil(telemetryCollectionStrategy.AWSLogs)
	logGroupPrefixes := []string{}
	for _, f := range telemetryCollectionStrategy.AWSLogs.Subscriptions {
		logGroupPrefixes = append(logGroupPrefixes, f.LogGroupNamePrefix)
	}
	require.Equal(1, len(logGroupPrefixes))
	require.True(strings.HasPrefix(logGroupPrefixes[0], "/aws/rds"))

	// change regions and update service configs and validate config changes for agent
	testAccountConfig2 := types.AccountConfig{
		EnabledRegions: []string{"us-east-2", "us-west-1"},
	}
	latestAccount := testbed.UpdateAccountConfigWithQS(
		"aws", testAccountId, testAccountConfig2,
	)
	require.Equal(testAccountId, latestAccount.ID.StringValue())
	require.Equal(testAccountConfig2, *latestAccount.Config)

	// disable metrics for one and logs for the other.
	// config should be as expected.
	setServiceConfig("ec2", false, false)
	setServiceConfig("rds", true, false)

	checkinResp = testbed.CheckInAsAgentWithQS(
		"aws", cloudintegrations.AgentCheckInRequest{
			ID:        testAccountId,
			AccountID: testAWSAccountId,
		},
	)
	require.Equal(testAccountId, checkinResp.AccountId)
	require.Equal(testAWSAccountId, checkinResp.CloudAccountId)
	require.Nil(checkinResp.RemovedAt)
	integrationConf = checkinResp.IntegrationConfig
	require.Equal(testAccountConfig2.EnabledRegions, integrationConf.EnabledRegions)

	telemetryCollectionStrategy = integrationConf.TelemetryCollectionStrategy
	require.Equal("aws", telemetryCollectionStrategy.Provider)
	require.NotNil(telemetryCollectionStrategy.AWSMetrics)
	metricStreamNamespaces = []string{}
	for _, f := range telemetryCollectionStrategy.AWSMetrics.StreamFilters {
		metricStreamNamespaces = append(metricStreamNamespaces, f.Namespace)
	}
	require.Equal([]string{"AWS/RDS"}, metricStreamNamespaces)

	require.NotNil(telemetryCollectionStrategy.AWSLogs)
	logGroupPrefixes = []string{}
	for _, f := range telemetryCollectionStrategy.AWSLogs.Subscriptions {
		logGroupPrefixes = append(logGroupPrefixes, f.LogGroupNamePrefix)
	}
	require.Equal(0, len(logGroupPrefixes))

}

type CloudIntegrationsTestBed struct {
	t              *testing.T
	testUser       *types.User
	qsHttpHandler  http.Handler
	mockClickhouse mockhouse.ClickConnMockCommon
	userModule     user.Module
}

// testDB can be injected for sharing a DB across multiple integration testbeds.
func NewCloudIntegrationsTestBed(t *testing.T, testDB sqlstore.SQLStore) *CloudIntegrationsTestBed {
	if testDB == nil {
		testDB = utils.NewQueryServiceDBForTests(t)
	}

	controller, err := cloudintegrations.NewController(testDB)
	if err != nil {
		t.Fatalf("could not create cloud integrations controller: %v", err)
	}

	reader, mockClickhouse := NewMockClickhouseReader(t, testDB)
	mockClickhouse.MatchExpectationsInOrder(false)

	providerSettings := instrumentationtest.New().ToProviderSettings()
	sharder, err := noopsharder.New(context.TODO(), providerSettings, sharder.Config{})
	require.NoError(t, err)
	orgGetter := implorganization.NewGetter(implorganization.NewStore(testDB), sharder)
	alertmanager, err := signozalertmanager.New(context.TODO(), providerSettings, alertmanager.Config{Signoz: alertmanager.Signoz{PollInterval: 10 * time.Second, Config: alertmanagerserver.NewConfig()}}, testDB, orgGetter)
	require.NoError(t, err)
	jwt := authtypes.NewJWT("", 1*time.Hour, 1*time.Hour)
	emailing := emailingtest.New()
	analytics := analyticstest.New()
	modules := signoz.NewModules(testDB, jwt, emailing, providerSettings, orgGetter, alertmanager, analytics)
	handlers := signoz.NewHandlers(modules)

	apiHandler, err := app.NewAPIHandler(app.APIHandlerOpts{
		Reader:                      reader,
		CloudIntegrationsController: controller,
		Signoz: &signoz.SigNoz{
			Modules:  modules,
			Handlers: handlers,
		},
	})
	if err != nil {
		t.Fatalf("could not create a new ApiHandler: %v", err)
	}

	router := app.NewRouter()
	router.Use(middleware.NewAuth(jwt, []string{"Authorization", "Sec-WebSocket-Protocol"}, sharder, instrumentationtest.New().Logger()).Wrap)
	am := middleware.NewAuthZ(instrumentationtest.New().Logger())
	apiHandler.RegisterRoutes(router, am)
	apiHandler.RegisterCloudIntegrationsRoutes(router, am)

	user, apiErr := createTestUser(modules.OrgSetter, modules.User)
	if apiErr != nil {
		t.Fatalf("could not create a test user: %v", apiErr)
	}

	return &CloudIntegrationsTestBed{
		t:              t,
		testUser:       user,
		qsHttpHandler:  router,
		mockClickhouse: mockClickhouse,
		userModule:     modules.User,
	}
}

func (tb *CloudIntegrationsTestBed) GetConnectedAccountsListFromQS(
	cloudProvider string,
) *cloudintegrations.ConnectedAccountsListResponse {
	respDataJson := tb.RequestQS(fmt.Sprintf("/api/v1/cloud-integrations/%s/accounts", cloudProvider), nil)

	var resp cloudintegrations.ConnectedAccountsListResponse
	err := json.Unmarshal(respDataJson, &resp)
	if err != nil {
		tb.t.Fatalf("could not unmarshal apiResponse.Data json into AccountsListResponse")
	}

	return &resp
}

func (tb *CloudIntegrationsTestBed) GenerateConnectionUrlFromQS(
	cloudProvider string, req cloudintegrations.GenerateConnectionUrlRequest,
) *cloudintegrations.GenerateConnectionUrlResponse {
	respDataJson := tb.RequestQS(
		fmt.Sprintf("/api/v1/cloud-integrations/%s/accounts/generate-connection-url", cloudProvider),
		req,
	)

	var resp cloudintegrations.GenerateConnectionUrlResponse
	err := json.Unmarshal(respDataJson, &resp)
	if err != nil {
		tb.t.Fatalf("could not unmarshal apiResponse.Data json into map[string]any")
	}

	return &resp
}

func (tb *CloudIntegrationsTestBed) GetAccountStatusFromQS(
	cloudProvider string, accountId string,
) *cloudintegrations.AccountStatusResponse {
	respDataJson := tb.RequestQS(fmt.Sprintf(
		"/api/v1/cloud-integrations/%s/accounts/%s/status",
		cloudProvider, accountId,
	), nil)

	var resp cloudintegrations.AccountStatusResponse
	err := json.Unmarshal(respDataJson, &resp)
	if err != nil {
		tb.t.Fatalf("could not unmarshal apiResponse.Data json into AccountStatusResponse")
	}

	return &resp
}

func (tb *CloudIntegrationsTestBed) CheckInAsAgentWithQS(
	cloudProvider string, req cloudintegrations.AgentCheckInRequest,
) *cloudintegrations.AgentCheckInResponse {
	respDataJson := tb.RequestQS(
		fmt.Sprintf("/api/v1/cloud-integrations/%s/agent-check-in", cloudProvider), req,
	)

	var resp cloudintegrations.AgentCheckInResponse
	err := json.Unmarshal(respDataJson, &resp)
	if err != nil {
		tb.t.Fatalf("could not unmarshal apiResponse.Data json into AgentCheckInResponse")
	}

	return &resp
}

func (tb *CloudIntegrationsTestBed) UpdateAccountConfigWithQS(
	cloudProvider string, accountId string, newConfig types.AccountConfig,
) *types.CloudIntegration {
	respDataJson := tb.RequestQS(
		fmt.Sprintf(
			"/api/v1/cloud-integrations/%s/accounts/%s/config",
			cloudProvider, accountId,
		), cloudintegrations.UpdateAccountConfigRequest{
			Config: newConfig,
		},
	)

	var resp types.CloudIntegration
	err := json.Unmarshal(respDataJson, &resp)
	if err != nil {
		tb.t.Fatalf("could not unmarshal apiResponse.Data json into Account")
	}

	return &resp
}

func (tb *CloudIntegrationsTestBed) DisconnectAccountWithQS(
	cloudProvider string, accountId string,
) *types.CloudIntegration {
	respDataJson := tb.RequestQS(
		fmt.Sprintf(
			"/api/v1/cloud-integrations/%s/accounts/%s/disconnect",
			cloudProvider, accountId,
		), map[string]any{},
	)

	var resp types.CloudIntegration
	err := json.Unmarshal(respDataJson, &resp)
	if err != nil {
		tb.t.Fatalf("could not unmarshal apiResponse.Data json into Account")
	}

	return &resp
}

func (tb *CloudIntegrationsTestBed) GetServicesFromQS(
	cloudProvider string, cloudAccountId *string,
) *cloudintegrations.ListServicesResponse {
	path := fmt.Sprintf("/api/v1/cloud-integrations/%s/services", cloudProvider)
	if cloudAccountId != nil {
		path = fmt.Sprintf("%s?cloud_account_id=%s", path, *cloudAccountId)
	}

	return RequestQSAndParseResp[cloudintegrations.ListServicesResponse](
		tb, path, nil,
	)
}

func (tb *CloudIntegrationsTestBed) GetServiceDetailFromQS(
	cloudProvider string, serviceId string, cloudAccountId *string,
) *cloudintegrations.ServiceDetails {
	path := fmt.Sprintf("/api/v1/cloud-integrations/%s/services/%s", cloudProvider, serviceId)
	if cloudAccountId != nil {
		path = fmt.Sprintf("%s?cloud_account_id=%s", path, *cloudAccountId)
	}

	// add mock expectations for connection status queries
	metricCols := []mockhouse.ColumnType{}
	metricCols = append(metricCols, mockhouse.ColumnType{Type: "String", Name: "metric_name"})
	metricCols = append(metricCols, mockhouse.ColumnType{Type: "String", Name: "labels"})
	metricCols = append(metricCols, mockhouse.ColumnType{Type: "Int64", Name: "unix_milli"})
	tb.mockClickhouse.ExpectQuery(
		`SELECT.*from.*signoz_metrics.*`,
	).WillReturnRows(mockhouse.NewRows(metricCols, [][]any{}))

	return RequestQSAndParseResp[cloudintegrations.ServiceDetails](
		tb, path, nil,
	)
}
func (tb *CloudIntegrationsTestBed) UpdateServiceConfigWithQS(
	cloudProvider string, serviceId string, req any,
) *cloudintegrations.UpdateServiceConfigResponse {
	path := fmt.Sprintf("/api/v1/cloud-integrations/%s/services/%s/config", cloudProvider, serviceId)

	return RequestQSAndParseResp[cloudintegrations.UpdateServiceConfigResponse](
		tb, path, req,
	)
}

func (tb *CloudIntegrationsTestBed) RequestQS(
	path string,
	postData interface{},
) (responseDataJson []byte) {
	req, err := AuthenticatedRequestForTest(
		tb.userModule, tb.testUser, path, postData,
	)
	if err != nil {
		tb.t.Fatalf("couldn't create authenticated test request: %v", err)
	}

	result, err := HandleTestRequest(tb.qsHttpHandler, req, 200)
	if err != nil {
		tb.t.Fatalf("test request failed: %v", err)
	}

	dataJson, err := json.Marshal(result.Data)
	if err != nil {
		tb.t.Fatalf("could not marshal apiResponse.Data: %v", err)
	}
	return dataJson
}

func RequestQSAndParseResp[ResponseType any](
	tb *CloudIntegrationsTestBed,
	path string,
	postData interface{},
) *ResponseType {
	respDataJson := tb.RequestQS(path, postData)

	var resp ResponseType

	err := json.Unmarshal(respDataJson, &resp)
	if err != nil {
		tb.t.Fatalf("could not unmarshal apiResponse.Data json into %T: %v", resp, err)
	}

	return &resp
}
