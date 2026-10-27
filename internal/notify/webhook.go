package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"hmoob-dock/internal/alerts"
	"hmoob-dock/internal/secret"
	"hmoob-dock/internal/store"
)

// The limits of a request to a target.
const (
	// RequestTimeout is the total time a request to a target may take.
	RequestTimeout = 10 * time.Second
	// MaxResponseBody is the number of the bytes of an answer that are
	// read at most.
	MaxResponseBody = 64 * 1024
	// maxURLLength is the longest address of a target that is accepted.
	maxURLLength = 2048
)

// userAgent identifies the control端 to the target.
const userAgent = "hmoob-dock-control"

// DefaultTargetID is the identifier of the target the administrator
// configures.
//
// The platform sends its notifications to one general target, so the
// administrator has one place to watch and one place to change.
const DefaultTargetID = "default"

// Resolver resolves the host of a target.
//
// The interface exists so the tests can answer with a fixed address
// instead of asking a name server.
type Resolver interface {
	// LookupIPAddr answers with the addresses of the host.
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// ConnectFunc opens the connection to an address that was checked.
//
// The type exists so the tests can watch the address the sender connects
// to without opening a connection.
type ConnectFunc func(ctx context.Context, network, address string) (net.Conn, error)

// WebhookConfig is the configuration of one target of the notifications.
type WebhookConfig struct {
	// ID identifies the target.
	ID string
	// URL is the address of the target.
	URL string
	// AllowedPrivateCIDRs are the private networks the administrator
	// allows the target to live in.
	AllowedPrivateCIDRs []string
	// Enabled reports whether the target is used.
	Enabled bool
}

// Sender sends the notifications of the alerts to one target.
type Sender struct {
	// config is the configuration of the target.
	config WebhookConfig
	// address is the parsed address of the target.
	address *url.URL
	// allowlist are the private networks the administrator allowed.
	allowlist []netip.Prefix
	// client sends the notifications.
	client *http.Client
	// resolver looks up the addresses of the host of the target.
	resolver Resolver
	// connect opens the connection to an address that was checked.
	connect ConnectFunc
}

// NewSender builds the sender of one target.
//
// The sender carries the policy of the addresses: it resolves the host of
// the target itself, checks every answer of the name and connects to the
// address it checked. The name of the host stays the name of the host
// for the certificate of the TLS, so the certificate of the target is
// verified as usual.
func NewSender(config WebhookConfig) (*Sender, error) {
	address, err := ParseTargetURL(config.URL)
	if err != nil {
		return nil, err
	}
	allowlist, err := parseAllowlist(config.AllowedPrivateCIDRs)
	if err != nil {
		return nil, err
	}
	sender := &Sender{
		config:    config,
		address:   address,
		allowlist: allowlist,
		resolver:  net.DefaultResolver,
	}
	dialer := &net.Dialer{Timeout: RequestTimeout}
	sender.connect = func(ctx context.Context, network, target string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, target)
	}
	sender.client = &http.Client{
		Timeout: RequestTimeout,
		// The target of a notification is the address the administrator
		// configured, so a redirect is refused instead of followed.
		CheckRedirect: CheckRedirect,
		Transport:     sender.transport(),
	}
	return sender, nil
}

// NewSenderWithClient builds a sender that sends through the given
// client.
//
// The tests use it to receive the notifications without a connection.
// The client of the production path carries the policy of the addresses,
// so a caller that brings its own client brings its own policy as well.
func NewSenderWithClient(config WebhookConfig, client *http.Client) (*Sender, error) {
	sender, err := NewSender(config)
	if err != nil {
		return nil, err
	}
	if client != nil {
		sender.client = client
	}
	return sender, nil
}

// UseResolver replaces the resolver of the sender.
//
// The tests use it to answer with a fixed address. The production path
// uses the resolver of the system.
func (s *Sender) UseResolver(resolver Resolver) *Sender {
	if resolver != nil {
		s.resolver = resolver
	}
	return s
}

// UseConnect replaces the function that opens the connection.
//
// The tests use it to watch the address the sender connects to without
// opening a connection. The production path opens the connection with
// the dialer of the system.
func (s *Sender) UseConnect(connect ConnectFunc) *Sender {
	if connect != nil {
		s.connect = connect
	}
	return s
}

// Timeout returns the total time a request to the target may take.
func (s *Sender) Timeout() time.Duration {
	return s.client.Timeout
}

// Send sends one notification to the target.
//
// The document carries the identity of the transition and nothing else:
// no YAML of an application, no variables of an application, no logs of a
// container and no credential of the platform. The receiver deduplicates
// the notifications by their identifier, and it tells an opening from a
// recovery by the transition of the event.
func (s *Sender) Send(ctx context.Context, message alerts.Notification) error {
	document, err := json.Marshal(newDocument(message))
	if err != nil {
		return fmt.Errorf("notify: encode the notification: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		s.address.String(), bytes.NewReader(document))
	if err != nil {
		// The reason of the error carries the address of the target, so
		// it is not part of the message.
		return errors.New("notify: the address of the target cannot be used")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", userAgent)

	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("notify: send the notification: %w", withoutAddress(err))
	}
	defer func() { _ = response.Body.Close() }()
	// The answer of the target is read within its limit, so a target
	// cannot make the control端 read an endless answer.
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, MaxResponseBody))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("notify: the target answered with the status %d", response.StatusCode)
	}
	return nil
}

// transport builds the transport of the sender.
//
// The transport carries the policy of the addresses. It resolves the host
// of the target itself and connects to the address it checked, so a name
// that answers with another address after the check cannot divert the
// notification. The environment of the control端 is not used as a proxy,
// because a proxy of the environment could send the notifications to
// another host.
func (s *Sender) transport() *http.Transport {
	return &http.Transport{
		Proxy:       nil,
		DialContext: s.dialContext,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		TLSHandshakeTimeout:   RequestTimeout,
		ResponseHeaderTimeout: RequestTimeout,
		ExpectContinueTimeout: time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
	}
}

// dialContext opens the connection to the host of the target.
//
// The name is resolved once and every answer is checked. The connection
// goes to an address that was checked, so a name that answers with
// another address after the check cannot divert the notification.
func (s *Sender) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("notify: the address %q is malformed: %w", address, err)
	}
	answers, err := s.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("notify: resolve %q: %w", host, err)
	}
	if len(answers) == 0 {
		return nil, fmt.Errorf("notify: the name %q has no address", host)
	}

	own := localAddresses()
	checked := make([]netip.Addr, 0, len(answers))
	for _, answer := range answers {
		parsed, ok := netip.AddrFromSlice(answer.IP)
		if !ok {
			continue
		}
		parsed = parsed.Unmap()
		if err := checkAddress(parsed, s.allowlist, own); err != nil {
			return nil, err
		}
		checked = append(checked, parsed)
	}
	if len(checked) == 0 {
		return nil, fmt.Errorf("notify: the name %q has no usable address", host)
	}

	var failure error
	for _, ip := range checked {
		conn, err := s.connect(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		failure = err
	}
	return nil, fmt.Errorf("notify: connect to %q: %w", host, failure)
}

// CheckRedirect refuses every redirect of a target.
//
// The target of a notification is the address the administrator
// configured, so a redirect is refused instead of followed.
func CheckRedirect(_ *http.Request, _ []*http.Request) error {
	return errors.New("notify: the target tried to redirect the notification")
}

// withoutAddress removes the address of the target from the error of a
// request.
//
// The error of a request carries the address of the target, and the
// address can carry a secret in its path, so only the reason of the
// failure travels on.
func withoutAddress(err error) error {
	var urlError *url.Error
	if errors.As(err, &urlError) && urlError.Err != nil {
		return urlError.Err
	}
	return err
}

// document is the JSON document a target receives.
type document struct {
	// NotificationID identifies the transition. The receiver
	// deduplicates the notifications by it.
	NotificationID string `json:"notification_id"`
	// EventID is the event of the transition.
	EventID string `json:"event_id"`
	// Transition is the number of the transition of the event. The
	// opening of an event and its recovery are two transitions.
	Transition int64 `json:"transition"`
	// Environment names the target of the event.
	Environment string `json:"environment"`
	// Object is the object of the event.
	Object string `json:"object"`
	// State is the state the event reached.
	State string `json:"state"`
	// OccurredAt is the time of the transition.
	OccurredAt string `json:"occurred_at"`
	// Summary describes the event.
	Summary string `json:"summary"`
}

// newDocument builds the document of a notification.
func newDocument(message alerts.Notification) document {
	return document{
		NotificationID: message.ID,
		EventID:        message.EventID,
		Transition:     message.Transition,
		Environment:    message.Environment,
		Object:         message.Object,
		State:          message.State,
		OccurredAt:     message.OccurredAt.UTC().Format(time.RFC3339Nano),
		Summary:        message.Summary,
	}
}

// ParseTargetURL parses the address of a target and checks it.
//
// Only the addresses of HTTP are accepted. A credential in the address
// and a fragment are refused, because both would end up in the log of the
// target or in the address of the connection, and a very long address is
// refused because it cannot be a real target.
func ParseTargetURL(raw string) (*url.URL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, errors.New("notify: the address of the target is empty")
	}
	if len(trimmed) > maxURLLength {
		return nil, fmt.Errorf("notify: the address of the target is longer than %d bytes", maxURLLength)
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		// The reason of the error carries the address of the target, so
		// it is not part of the message.
		return nil, errors.New("notify: the address of the target is malformed")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("notify: the address of the target must use HTTP or HTTPS")
	}
	if parsed.Host == "" {
		return nil, errors.New("notify: the address of the target names no host")
	}
	if parsed.User != nil {
		return nil, errors.New("notify: the address of the target must not carry a credential")
	}
	if parsed.Fragment != "" {
		return nil, errors.New("notify: the address of the target must not carry a fragment")
	}
	return parsed, nil
}

// MaskURL returns the display form of the address of a target.
//
// The address of a service of a chat holds a token in its path, so the
// display form keeps the host and hides everything behind it.
func MaskURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" {
		return ""
	}
	masked := parsed.Scheme + "://" + parsed.Host
	if parsed.Path != "" && parsed.Path != "/" {
		masked += "/***"
	}
	return masked
}

// Target is the configuration of one target of the notifications.
type Target struct {
	// ID identifies the target.
	ID string
	// URL is the address of the target. It is empty when the target was
	// never configured.
	URL string
	// MaskedURL is the display form of the address. It holds no secret.
	MaskedURL string
	// AllowedPrivateCIDRs are the private networks the administrator
	// allowed the target to live in.
	AllowedPrivateCIDRs []string
	// Enabled reports whether the target is used.
	Enabled bool
	// CreatedAt is the time of the creation of the target.
	CreatedAt time.Time
	// UpdatedAt is the time of the last change of the target.
	UpdatedAt time.Time
}

// Store reads and writes the targets of the notifications.
//
// The address of a target is stored encrypted, because it can carry a
// secret in its path or in its query. Only the masked form of the
// address is stored in the clear, so the list of the targets can be
// answered without opening the secret box.
type Store struct {
	// db is the write pool of the control端.
	db *sql.DB
	// box seals the addresses of the targets.
	box *secret.Box
}

// NewStore opens the store of the targets on the database.
func NewStore(db *sql.DB, box *secret.Box) *Store {
	return &Store{db: db, box: box}
}

// Targets returns the configured targets.
func (s *Store) Targets(ctx context.Context) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, url_ciphertext, url_masked, allowed_private_cidrs, enabled, created_at, updated_at
		  FROM webhook_targets ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("notify: read the targets: %w", err)
	}
	defer rows.Close()

	var targets []Target
	for rows.Next() {
		target, err := s.scanTarget(rows)
		if err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("notify: read the targets: %w", err)
	}
	return targets, nil
}

// Target returns one target.
//
// A target that was never configured is reported as missing, so the
// caller can tell it from a target that cannot be read.
func (s *Store) Target(ctx context.Context, id string) (Target, bool, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, url_ciphertext, url_masked, allowed_private_cidrs, enabled, created_at, updated_at
		  FROM webhook_targets WHERE id = ?`, id)
	target, err := s.scanTarget(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, err
	}
	return target, true, nil
}

// scanTarget reads one target of a row.
func (s *Store) scanTarget(row scanner) (Target, error) {
	var target Target
	var ciphertext []byte
	var networks string
	var enabled int
	var createdAt, updatedAt int64
	if err := row.Scan(&target.ID, &ciphertext, &target.MaskedURL, &networks,
		&enabled, &createdAt, &updatedAt); err != nil {
		return Target{}, fmt.Errorf("notify: read a target: %w", err)
	}
	target.Enabled = enabled != 0
	target.CreatedAt = store.TimeFromMillis(createdAt)
	target.UpdatedAt = store.TimeFromMillis(updatedAt)
	target.AllowedPrivateCIDRs = splitNetworks(networks)

	plain, err := s.box.Open(ciphertext, targetAAD(target.ID))
	if err != nil {
		return Target{}, fmt.Errorf("notify: decrypt the address of the target %s: %w", target.ID, err)
	}
	target.URL = string(plain)
	return target, nil
}

// Save writes one target.
//
// The address of the target is sealed before it is written, because it
// can carry a secret. The masked form is written next to it, so the list
// of the targets never has to open the secret box.
func (s *Store) Save(ctx context.Context, target Target) error {
	if strings.TrimSpace(target.ID) == "" {
		return errors.New("notify: the target has no identifier")
	}
	sealed, err := s.box.Seal([]byte(target.URL), targetAAD(target.ID))
	if err != nil {
		return fmt.Errorf("notify: seal the address of the target %s: %w", target.ID, err)
	}
	now := store.TimeToMillis(time.Now().UTC())
	enabled := 0
	if target.Enabled {
		enabled = 1
	}
	networks := strings.Join(target.AllowedPrivateCIDRs, ",")
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO webhook_targets
		    (id, url_ciphertext, url_masked, allowed_private_cidrs, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET
		    url_ciphertext = excluded.url_ciphertext,
		    url_masked = excluded.url_masked,
		    allowed_private_cidrs = excluded.allowed_private_cidrs,
		    enabled = excluded.enabled,
		    updated_at = excluded.updated_at`,
		target.ID, sealed, MaskURL(target.URL), networks, enabled, now, now); err != nil {
		return fmt.Errorf("notify: write the target %s: %w", target.ID, err)
	}
	return nil
}

// targetAAD binds the address of a target to its identifier.
func targetAAD(id string) []byte {
	return []byte("hmoob-dock:webhook:" + id)
}

// splitNetworks reads the networks of a target.
func splitNetworks(value string) []string {
	var networks []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			networks = append(networks, trimmed)
		}
	}
	return networks
}

// scanner is the part of the row types the target scan needs.
type scanner interface {
	Scan(dest ...any) error
}
