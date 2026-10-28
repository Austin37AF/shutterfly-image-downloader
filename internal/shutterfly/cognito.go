package shutterfly

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// CognitoBase overrides the Cognito endpoint host for tests. Empty = derive
// from the token's iss claim.

// cognitoRegion extracts the region from an iss URL like
// https://cognito-idp.us-east-1.amazonaws.com/<pool>.
func cognitoRegion(iss string) string {
	u, err := url.Parse(iss)
	if err != nil {
		return ""
	}
	parts := strings.Split(u.Host, ".")
	if len(parts) >= 2 && parts[0] == "cognito-idp" {
		return parts[1]
	}
	return ""
}

// refreshFilePermsOK requires 0600 (no group/world bits).
func refreshFilePermsOK(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("refresh token file %s must be 0600 (is %o)", path, fi.Mode().Perm())
	}
	return nil
}

// refreshViaCognitoLocked attempts InitiateAuth. Caller holds c.mu.
func (c *Client) refreshViaCognitoLocked() (string, string, error) {

	iss, _ := c.claims["iss"].(string)
	aud, _ := c.claims["aud"].(string)
	region := cognitoRegion(iss)
	if region == "" || aud == "" {
		return "", "", fmt.Errorf("token lacks iss/aud for refresh")
	}
	rtPath := c.RefreshFile
	if rtPath == "" {
		return "", "", fmt.Errorf("no refresh file configured")
	}
	if err := refreshFilePermsOK(rtPath); err != nil {
		return "", "", err
	}
	rtBytes, err := os.ReadFile(rtPath)
	if err != nil {
		return "", "", err
	}
	refreshTok := strings.TrimSpace(string(rtBytes))
	if refreshTok == "" {
		return "", "", fmt.Errorf("empty refresh token file")
	}

	base := c.CognitoBase
	if base == "" {
		base = "https://cognito-idp." + region + ".amazonaws.com"
	}
	body, _ := json.Marshal(map[string]interface{}{
		"AuthFlow":       "REFRESH_TOKEN_AUTH",
		"ClientId":       aud,
		"AuthParameters": map[string]string{"REFRESH_TOKEN": refreshTok},
	})
	req, err := http.NewRequest("POST", base+"/", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", "AWSCognitoIdentityProviderService.InitiateAuth")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)

	var result struct {
		AuthenticationResult struct {
			IdToken      string `json:"IdToken"`
			AccessToken  string `json:"AccessToken"`
			RefreshToken string `json:"RefreshToken"`
		} `json:"AuthenticationResult"`
		Type    string `json:"__type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return "", "", err
	}
	if resp.StatusCode != 200 || result.AuthenticationResult.IdToken == "" {
		errType := result.Type
		if errType == "" {
			errType = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		// strip #suffix from __type
		if i := strings.LastIndex(errType, "#"); i >= 0 {
			errType = errType[i+1:]
		}
		msg := ""
		if errType == "ForbiddenException" && result.Message != "" {
			msg = ": " + result.Message
		}
		return "", "", fmt.Errorf("cognito refresh failed: %s%s", errType, msg)
	}

	newRefresh := result.AuthenticationResult.RefreshToken
	if newRefresh != "" && newRefresh != refreshTok {
		tmp := rtPath + ".tmp"
		if err := os.WriteFile(tmp, []byte(newRefresh+"\n"), 0600); err == nil {
			os.Chmod(tmp, 0600)
			os.Rename(tmp, rtPath)
		}
	}
	return result.AuthenticationResult.IdToken, "", nil
}

// RefreshTest performs one Cognito refresh and prints a one-line result.
func (c *Client) RefreshTest() error {
	tok, _, err := c.refreshViaCognitoLocked()
	if err != nil {
		fmt.Println("refresh failed:", err)
		return err
	}
	c.setToken(tok)
	_, exp := parseToken(tok)
	fmt.Printf("refresh OK, new token expires %s\n", exp.Format(time.RFC3339))
	return nil
}
