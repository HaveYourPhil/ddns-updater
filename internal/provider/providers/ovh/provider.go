package ovh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/qdm12/ddns-updater/internal/models"
	"github.com/qdm12/ddns-updater/internal/provider/constants"
	"github.com/qdm12/ddns-updater/internal/provider/errors"
	"github.com/qdm12/ddns-updater/internal/provider/headers"
	"github.com/qdm12/ddns-updater/internal/provider/utils"
	"github.com/qdm12/ddns-updater/pkg/publicip/ipversion"
)

type Provider struct {
	domain        string
	host          string
	ipVersion     ipversion.IPVersion
	username      string
	password      string
	useProviderIP bool
	mode          string
	apiURL        *url.URL
	appKey        string
	appSecret     string
	consumerKey   string
	timeNow       func() time.Time
	serverDelta   time.Duration
}

func New(data json.RawMessage, domain, host string,
	ipVersion ipversion.IPVersion) (p *Provider, err error) {
	extraSettings := struct {
		Username      string `json:"username"`
		Password      string `json:"password"`
		UseProviderIP bool   `json:"provider_ip"`
		Mode          string `json:"mode"`
		APIEndpoint   string `json:"api_endpoint"`
		AppKey        string `json:"app_key"`
		AppSecret     string `json:"app_secret"`
		ConsumerKey   string `json:"consumer_key"`
	}{}
	err = json.Unmarshal(data, &extraSettings)
	if err != nil {
		return nil, err
	}

	apiURL, err := convertShortEndpoint(extraSettings.APIEndpoint)
	if err != nil {
		return nil, err
	}

	p = &Provider{
		domain:        domain,
		host:          host,
		ipVersion:     ipVersion,
		username:      extraSettings.Username,
		password:      extraSettings.Password,
		useProviderIP: extraSettings.UseProviderIP,
		mode:          extraSettings.Mode,
		apiURL:        apiURL,
		appKey:        extraSettings.AppKey,
		appSecret:     extraSettings.AppSecret,
		consumerKey:   extraSettings.ConsumerKey,
		timeNow:       time.Now,
	}
	err = p.isValid()
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Provider) isValid() error {
	if p.mode == "api" {
		switch {
		case p.appKey == "":
			return fmt.Errorf("%w", errors.ErrAppKeyNotSet)
		case p.consumerKey == "":
			return fmt.Errorf("%w", errors.ErrConsumerKeyNotSet)
		case p.appSecret == "":
			return fmt.Errorf("%w", errors.ErrSecretNotSet)
		}
	} else {
		switch {
		case p.username == "":
			return fmt.Errorf("%w", errors.ErrUsernameNotSet)
		case p.password == "":
			return fmt.Errorf("%w", errors.ErrPasswordNotSet)
		case p.host == "*":
			return fmt.Errorf("%w", errors.ErrHostWildcard)
		}
	}
	return nil
}

func (p *Provider) String() string {
	return fmt.Sprintf("[domain: %s | host: %s | provider: OVH]", p.domain, p.host)
}

func (p *Provider) Domain() string {
	return p.domain
}

func (p *Provider) Host() string {
	return p.host
}

func (p *Provider) IPVersion() ipversion.IPVersion {
	return p.ipVersion
}

func (p *Provider) Proxied() bool {
	return false
}

func (p *Provider) BuildDomainName() string {
	return utils.BuildDomainName(p.host, p.domain)
}

func (p *Provider) HTML() models.HTMLRow {
	return models.HTMLRow{
		Domain:    fmt.Sprintf("<a href=\"http://%s\">%s</a>", p.BuildDomainName(), p.BuildDomainName()),
		Host:      p.Host(),
		Provider:  "<a href=\"https://www.ovh.com/\">OVH DNS</a>",
		IPVersion: p.ipVersion.String(),
	}
}

func (p *Provider) updateWithDynHost(ctx context.Context, client *http.Client,
	ip netip.Addr) (newIP netip.Addr, err error) {
	u := url.URL{
		Scheme: "https",
		User:   url.UserPassword(p.username, p.password),
		Host:   "www.ovh.com",
		Path:   "/nic/update",
	}
	values := url.Values{}
	values.Set("system", "dyndns")
	values.Set("hostname", utils.BuildURLQueryHostname(p.host, p.domain))
	if !p.useProviderIP {
		values.Set("myip", ip.String())
	}
	u.RawQuery = values.Encode()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("creating http request: %w", err)
	}
	headers.SetUserAgent(request)

	response, err := client.Do(request)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("doing http request: %w", err)
	}
	defer response.Body.Close()

	b, err := io.ReadAll(response.Body)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("reading response body: %w", err)
	}
	s := string(b)

	if response.StatusCode != http.StatusOK {
		return netip.Addr{}, fmt.Errorf("%w: %d: %s", errors.ErrHTTPStatusNotValid, response.StatusCode, s)
	}

	switch {
	case strings.HasPrefix(s, constants.Notfqdn):
		return netip.Addr{}, fmt.Errorf("%w", errors.ErrHostnameNotExists)
	case strings.HasPrefix(s, "badrequest"):
		return netip.Addr{}, fmt.Errorf("%w", errors.ErrBadRequest)
	case strings.HasPrefix(s, "nochg"):
		return ip, nil
	case strings.HasPrefix(s, "good"):
		return ip, nil
	default:
		return netip.Addr{}, fmt.Errorf("%w: %s", errors.ErrUnknownResponse, s)
	}
}

func (p *Provider) updateWithZoneDNS(ctx context.Context, client *http.Client, ip netip.Addr) (
	newIP netip.Addr, err error) {
	ipStr := ip.Unmap().String()
	recordType := constants.A
	if ip.Is6() {
		recordType = constants.AAAA
	}
	// subDomain filter of the ovh api expect an empty string to get @ record
	subDomain := p.host
	if subDomain == "@" {
		subDomain = ""
	}

	timestamp, err := p.getAdjustedUnixTimestamp(ctx, client)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("obtain adjusted time from OVH: %w", err)
	}

	recordIDs, err := p.getRecords(ctx, client, recordType, subDomain, timestamp)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("listing records: %w", err)
	}

	if len(recordIDs) == 0 {
		err = p.createRecord(ctx, client, recordType, subDomain, ipStr, timestamp)
		if err != nil {
			return netip.Addr{}, fmt.Errorf("creating record: %w", err)
		}
	} else {
		for _, recordID := range recordIDs {
			err = p.updateRecord(ctx, client, recordID, ipStr, timestamp)
			if err != nil {
				return netip.Addr{}, fmt.Errorf("updating record: %w", err)
			}
		}
	}

	err = p.refresh(ctx, client, timestamp)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("refreshing records: %w", err)
	}

	return ip, nil
}

func (p *Provider) Update(ctx context.Context, client *http.Client, ip netip.Addr) (newIP netip.Addr, err error) {
	if p.mode != "api" {
		return p.updateWithDynHost(ctx, client, ip)
	}
	return p.updateWithZoneDNS(ctx, client, ip)
}
