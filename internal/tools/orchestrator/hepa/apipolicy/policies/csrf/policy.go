// Copyright (c) 2021 Terminus, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package custom

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/erda-project/erda/internal/tools/orchestrator/hepa/apipolicy"
	"github.com/erda-project/erda/internal/tools/orchestrator/hepa/kong"
	kongDto "github.com/erda-project/erda/internal/tools/orchestrator/hepa/kong/dto"
	"github.com/erda-project/erda/internal/tools/orchestrator/hepa/repository/orm"
	db "github.com/erda-project/erda/internal/tools/orchestrator/hepa/repository/service"
)

const (
	PolicyName = "safety-csrf"
	PluginName = "csrf-token"
)

func init() {
	if err := apipolicy.RegisterPolicyEngine(PolicyName, new(Policy)); err != nil {
		panic(err)
	}
}

type Policy struct {
	apipolicy.BasePolicy
}

func (policy Policy) CreateDefaultConfig(ctx map[string]interface{}) apipolicy.PolicyDto {
	value, ok := ctx[apipolicy.CTX_SERVICE_INFO]
	if !ok {
		logrus.Errorf("get identify failed:%+v", ctx)
		return nil
	}
	info, ok := value.(apipolicy.ServiceInfo)
	if !ok {
		logrus.Errorf("convert failed:%+v", value)
		return nil
	}
	tokenName := strings.ToLower(fmt.Sprintf("x-%s-%s-csrf-token", strings.Replace(info.ProjectName, "_", "-", -1), info.Env))
	dto := &PolicyDto{
		ExcludedMethod: []string{"GET", "HEAD", "OPTIONS", "TRACE"},
		TokenName:      tokenName,
		CookieSecure:   false,
		ValidTTL:       1800,
		RefreshTTL:     10,
		ErrStatus:      403,
		ErrMsg:         `{"message":"This form has expired. Please refresh and try again."}`,
	}
	dto.Switch = false
	return dto
}

func (policy Policy) UnmarshalConfig(config []byte) (apipolicy.PolicyDto, error, string) {
	var policyDto PolicyDto
	if err := json.Unmarshal(config, &policyDto); err != nil {
		return nil, errors.Wrapf(err, "failed to Unmarshal config: %s", config), "Invalid config"
	}
	if ok, msg := policyDto.IsValidDto(); !ok {
		return nil, errors.Errorf("invalid policy dto, msg:%s", msg), msg
	}
	return &policyDto, nil, ""
}

func (policy Policy) buildPluginReq(dto *PolicyDto) *kongDto.KongPluginReqDto {
	req := &kongDto.KongPluginReqDto{
		Name:    PluginName,
		Config:  map[string]interface{}{},
		Enabled: &dto.Switch,
	}
	req.Config["biz_cookie"] = []string{dto.UserCookie}
	if dto.TokenDomain != "" {
		req.Config["biz_domain"] = dto.TokenDomain
	}
	req.Config["excluded_method"] = dto.ExcludedMethod
	req.Config["token_key"] = dto.TokenName
	req.Config["token_cookie"] = dto.TokenName
	req.Config["secure_cookie"] = dto.CookieSecure
	req.Config["valid_ttl"] = dto.ValidTTL
	req.Config["refresh_ttl"] = dto.RefreshTTL
	req.Config["err_status"] = dto.ErrStatus
	req.Config["err_message"] = dto.ErrMsg
	sha := sha256.New()
	_, _ = sha.Write([]byte(dto.TokenName + ":secret"))
	tokenSecret := fmt.Sprintf("%x", sha.Sum(nil))
	req.Config["jwt_secret"] = tokenSecret[:16] + tokenSecret[48:]

	return req
}

// ParseConfig is used to parse the policy configuration .
func (policy Policy) ParseConfig(dto apipolicy.PolicyDto, ctx map[string]interface{}) (apipolicy.PolicyConfig, error) {
	l := logrus.WithField("pluginName", PluginName).WithField("func", "ParseConfig")
	l.Infof("dto: %+v", dto)
	res := apipolicy.PolicyConfig{}
	policyDto, ok := dto.(*PolicyDto)
	if !ok {
		return res, errors.Errorf("invalid config:%+v", dto)
	}
	adapter, ok := ctx[apipolicy.CTX_KONG_ADAPTER].(kong.KongAdapter)
	if !ok {
		return res, errors.Errorf("failed to get identify with %s: %+v", apipolicy.CTX_KONG_ADAPTER, ctx)
	}
	kongVersion, err := adapter.GetVersion()
	if err != nil {
		return res, errors.Wrap(err, "failed to retrieve Kong version")
	}
	if !strings.HasPrefix(kongVersion, "2.") {
		return res, errors.Errorf("the plugin %s is not supportted on the Kong version %s", PluginName, kongVersion)
	}
	zone, ok := ctx[apipolicy.CTX_ZONE].(*orm.GatewayZone)
	if !ok {
		return res, errors.Errorf("failed to get identify with %s: %+v", apipolicy.CTX_ZONE, ctx)
	}
	policyDb, _ := db.NewGatewayPolicyServiceImpl()
	exist, err := policyDb.GetByAny(&orm.GatewayPolicy{
		ZoneId:     zone.Id,
		PluginName: PluginName,
	})
	if err != nil {
		return res, err
	}
	if !policyDto.Switch {
		if exist != nil {
			err = adapter.RemovePlugin(exist.PluginId)
			if err != nil {
				return res, err
			}
			_ = policyDb.DeleteById(exist.Id)
			res.KongPolicyChange = true
		}
		return res, nil
	}
	req := policy.buildPluginReq(policyDto)
	if exist != nil {
		req.Id = exist.PluginId
		resp, err := adapter.CreateOrUpdatePluginById(req)
		if err != nil {
			return res, err
		}
		configByte, err := json.Marshal(resp.Config)
		if err != nil {
			return res, err
		}
		exist.Config = configByte
		err = policyDb.Update(exist)
		if err != nil {
			return res, err
		}
	} else {
		resp, err := adapter.AddPlugin(req)
		if err != nil {
			return res, err
		}
		configByte, err := json.Marshal(resp.Config)
		if err != nil {
			return res, err
		}
		policyDao := &orm.GatewayPolicy{
			ZoneId:     zone.Id,
			PluginName: PluginName,
			Category:   "safety",
			PluginId:   resp.Id,
			Config:     configByte,
			Enabled:    1,
		}
		err = policyDb.Insert(policyDao)
		if err != nil {
			return res, err
		}
		res.KongPolicyChange = true
	}
	return res, nil
}
