// +build ignore

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

type SessionInfo struct {
	Token     string                 `json:"token"`
	SessionID string                 `json:"sessionId"`
}

type Config struct {
	Host     string `json:"host"`
}

type ApplicationCreateRequest struct {
	Name        string `json:"name"`
	Mode        string `json:"mode"`
	Desc        string `json:"desc"`
	ProjectID   uint64 `json:"projectId"`
}

type BaseResponse struct {
	Success bool        `json:"success"`
	Data    interface{} `json:"data"`
	Err     interface{} `json:"err"`
}

type ApplicationDTO struct {
	ID   uint64 `json:"id"`
	Name string `json:"name"`
}

type ApplicationCreateResponse struct {
	BaseResponse
	Data ApplicationDTO `json:"data"`
}

type ProjectListResponse struct {
	Success bool `json:"success"`
	Data    struct {
		List []struct {
			ID   uint64 `json:"id"`
			Name string `json:"name"`
		} `json:"list"`
	} `json:"data"`
}

func main() {
	if len(os.Args) < 4 {
		fmt.Println("Usage: go run create-apps.go <org-name> <project-name> <app1> <app2> ...")
		return
	}

	orgName := os.Args[1]
	projectName := os.Args[2]
	appNames := os.Args[3:]

	// Load config and sessions
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".erda.d")

	// Read config
	var cfg Config
	configFile := filepath.Join(configDir, "config")
	data, _ := os.ReadFile(configFile)
	json.Unmarshal(data, &cfg)

	// Read sessions
	sessions := make(map[string]SessionInfo)
	sessionsFile := filepath.Join(configDir, "sessions")
	data, _ = os.ReadFile(sessionsFile)
	json.Unmarshal(data, &sessions)

	// Fetch openapi
	client := &http.Client{}
	u, _ := url.Parse(cfg.Host)
	resp, _ := client.Get(fmt.Sprintf("%s://%s/metadata.json", u.Scheme, u.Host))
	var metadata struct {
		OpenapiPublicUrl string `json:"openapi_public_url"`
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	json.Unmarshal(body, &metadata)
	openapi, _ := url.Parse(metadata.OpenapiPublicUrl)

	// Get session
	var session SessionInfo
	if s, ok := sessions[openapi.String()]; ok {
		session = s
	} else if s, ok := sessions[cfg.Host]; ok {
		session = s
	}

	// Get org ID (we'll skip for now, just need project ID)
	// Get project ID
	projReq, _ := http.NewRequest("GET", openapi.String()+fmt.Sprintf("/api/projects?org=%s&joined=true&pageSize=100", orgName), nil)
	projReq.Header.Set("Content-Type", "application/json")
	projReq.Header.Set("USE-TOKEN", "true")
	if session.Token != "" {
		projReq.Header.Set("Authorization", session.Token)
	}
	if session.SessionID != "" {
		projReq.AddCookie(&http.Cookie{Name: "OPENAPISESSION", Value: session.SessionID})
	}

	projResp, _ := client.Do(projReq)
	projBody, _ := io.ReadAll(projResp.Body)
	projResp.Body.Close()

	var projList ProjectListResponse
	json.Unmarshal(projBody, &projList)

	var projectID uint64
	for _, p := range projList.Data.List {
		if p.Name == projectName {
			projectID = p.ID
			break
		}
	}

	if projectID == 0 {
		fmt.Printf("Project %s not found\n", projectName)
		return
	}

	fmt.Printf("Found project %s (ID: %d)\n", projectName, projectID)

	// Create apps
	for _, appName := range appNames {
		fmt.Printf("Creating app %s... ", appName)

		reqBody := ApplicationCreateRequest{
			Name:      appName,
			Mode:      "SERVICE",
			Desc:      "Test app for batch grant",
			ProjectID: projectID,
		}

		reqData, _ := json.Marshal(reqBody)
		req, _ := http.NewRequest("POST", openapi.String()+"/api/applications", bytes.NewReader(reqData))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("USE-TOKEN", "true")
		req.Header.Set("org", orgName)
		if session.Token != "" {
			req.Header.Set("Authorization", session.Token)
		}
		if session.SessionID != "" {
			req.AddCookie(&http.Cookie{Name: "OPENAPISESSION", Value: session.SessionID})
		}

		resp, err := client.Do(req)
		if err != nil {
			fmt.Printf("FAIL: %v\n", err)
			continue
		}
		defer resp.Body.Close()

		respData, _ := io.ReadAll(resp.Body)
		var appResp ApplicationCreateResponse
		json.Unmarshal(respData, &appResp)

		if resp.StatusCode >= 200 && resp.StatusCode < 300 && appResp.Success {
			fmt.Printf("OK (ID: %d)\n", appResp.Data.ID)
		} else {
			fmt.Printf("FAIL: %s\n", string(respData))
		}
	}
}
