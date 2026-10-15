package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"

	"github.com/langhua98/xiaoju-cloud/internal/meta"
)

const (
	ceremonyTTL   = 3 * time.Minute  // 一次刷脸 / 注册从开始到完成的最长时间
	addCodeTTL    = 10 * time.Minute // 添加新设备的验证码有效期
	maxCeremonies = 200
	maxCodeFails  = 5 // 添加码连续输错这么多次后，现有添加码全部作废
)

var (
	ErrUnavailable  = errors.New("passkeys are not configured on this server (XIAOJU_WEBAUTHN_RP_ID / CORS origins)")
	ErrCeremony     = errors.New("verification expired, please try again")
	ErrVerification = errors.New("passkey verification failed")
	ErrNotAllowed   = errors.New("adding a device requires a valid add-device code")
)

type PasskeyConfig struct {
	RPID    string   // 通行密钥绑定的域名，必须是网页所在域名（或其上级域名）
	Origins []string // 允许发起验证的网页来源，如 https://user.github.io
}

// Passkeys 管理通行密钥（Face ID / 指纹 / Windows Hello）的注册与验证。服务端只保存公钥。
type Passkeys struct {
	wa     *webauthn.WebAuthn // nil 表示未配置，无法注册和验证
	store  *meta.Store
	userID []byte

	mu         sync.Mutex
	ceremonies map[string]ceremony
	addCodes   map[string]time.Time
	codeFails  int
	count      int
}

type ceremony struct {
	kind string // login | register
	data webauthn.SessionData
	name string
	exp  time.Time
}

// owner 是唯一的网盘主人（实现 webauthn.User）。
type owner struct {
	id    []byte
	creds []webauthn.Credential
}

func (o *owner) WebAuthnID() []byte                         { return o.id }
func (o *owner) WebAuthnName() string                       { return "xiaoju-owner" }
func (o *owner) WebAuthnDisplayName() string                { return "小桔云盘主人" }
func (o *owner) WebAuthnCredentials() []webauthn.Credential { return o.creds }

func NewPasskeys(ctx context.Context, store *meta.Store, userID uuid.UUID, cfg PasskeyConfig) (*Passkeys, error) {
	p := &Passkeys{
		store:      store,
		userID:     userID[:],
		ceremonies: map[string]ceremony{},
		addCodes:   map[string]time.Time{},
	}
	if cfg.RPID != "" && len(cfg.Origins) > 0 {
		wa, err := webauthn.New(&webauthn.Config{
			RPID:          cfg.RPID,
			RPDisplayName: "小桔云盘",
			RPOrigins:     cfg.Origins,
		})
		if err != nil {
			return nil, fmt.Errorf("webauthn config: %w", err)
		}
		p.wa = wa
	}
	n, err := store.CountPasskeys(ctx)
	if err != nil {
		return nil, err
	}
	p.count = n
	return p, nil
}

// Available 报告服务端是否配置了通行密钥。
func (p *Passkeys) Available() bool { return p.wa != nil }

// Required 报告是否已经注册了设备：注册后，仅凭访问令牌不能再访问文件，必须刷脸。
func (p *Passkeys) Required() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.count > 0
}

func (p *Passkeys) owner(ctx context.Context) (*owner, error) {
	rows, err := p.store.ListPasskeys(ctx)
	if err != nil {
		return nil, err
	}
	o := &owner{id: p.userID}
	for _, r := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(r.Credential, &c); err != nil {
			return nil, fmt.Errorf("decode passkey %x: %w", r.ID, err)
		}
		o.creds = append(o.creds, c)
	}
	return o, nil
}

func (p *Passkeys) putCeremony(c ceremony) (string, error) {
	id, err := randomString(18)
	if err != nil {
		return "", err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, v := range p.ceremonies {
		if now.After(v.exp) {
			delete(p.ceremonies, k)
		}
	}
	if len(p.ceremonies) >= maxCeremonies {
		return "", errors.New("too many pending verifications, please retry later")
	}
	c.exp = now.Add(ceremonyTTL)
	p.ceremonies[id] = c
	return id, nil
}

// takeCeremony 取出并作废一次验证（一次性）。
func (p *Passkeys) takeCeremony(id, kind string) (ceremony, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.ceremonies[id]
	delete(p.ceremonies, id)
	if !ok || c.kind != kind || time.Now().After(c.exp) {
		return ceremony{}, ErrCeremony
	}
	return c, nil
}

// BeginLogin 开始刷脸验证，返回验证 ID 和要交给浏览器 navigator.credentials.get 的参数。
func (p *Passkeys) BeginLogin(ctx context.Context) (string, *protocol.CredentialAssertion, error) {
	if p.wa == nil {
		return "", nil, ErrUnavailable
	}
	o, err := p.owner(ctx)
	if err != nil {
		return "", nil, err
	}
	if len(o.creds) == 0 {
		return "", nil, errors.New("no device registered yet")
	}
	opts, data, err := p.wa.BeginLogin(o, webauthn.WithUserVerification(protocol.VerificationRequired))
	if err != nil {
		return "", nil, err
	}
	id, err := p.putCeremony(ceremony{kind: "login", data: *data})
	return id, opts, err
}

// FinishLogin 校验浏览器返回的签名（必须经过 Face ID / 指纹 / 锁屏密码验证）。
func (p *Passkeys) FinishLogin(ctx context.Context, ceremonyID string, body io.Reader) error {
	if p.wa == nil {
		return ErrUnavailable
	}
	c, err := p.takeCeremony(ceremonyID, "login")
	if err != nil {
		return err
	}
	parsed, err := protocol.ParseCredentialRequestResponseBody(body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerification, err)
	}
	o, err := p.owner(ctx)
	if err != nil {
		return err
	}
	cred, err := p.wa.ValidateLogin(o, c.data, parsed)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerification, err)
	}
	if cred.Authenticator.CloneWarning {
		return fmt.Errorf("%w: signature counter went backwards (possible cloned key)", ErrVerification)
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	return p.store.TouchPasskey(ctx, cred.ID, raw)
}

// BeginRegistration 开始在当前设备上注册 Face ID / 指纹。authorized 由调用方决定
// （首个设备只需令牌；之后需要已登录的会话或添加码）。
func (p *Passkeys) BeginRegistration(ctx context.Context, name string) (string, *protocol.CredentialCreation, error) {
	if p.wa == nil {
		return "", nil, ErrUnavailable
	}
	o, err := p.owner(ctx)
	if err != nil {
		return "", nil, err
	}
	exclude := make([]protocol.CredentialDescriptor, len(o.creds))
	for i := range o.creds {
		exclude[i] = o.creds[i].Descriptor()
	}
	opts, data, err := p.wa.BeginRegistration(o,
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{
			AuthenticatorAttachment: protocol.Platform, // 本机的 Face ID / 指纹 / Windows Hello
			ResidentKey:             protocol.ResidentKeyRequirementPreferred,
			UserVerification:        protocol.VerificationRequired,
		}),
		webauthn.WithConveyancePreference(protocol.PreferNoAttestation),
		webauthn.WithExclusions(exclude),
	)
	if err != nil {
		return "", nil, err
	}
	id, err := p.putCeremony(ceremony{kind: "register", data: *data, name: name})
	return id, opts, err
}

// FinishRegistration 校验并保存新设备的公钥。
func (p *Passkeys) FinishRegistration(ctx context.Context, ceremonyID string, body io.Reader) error {
	if p.wa == nil {
		return ErrUnavailable
	}
	c, err := p.takeCeremony(ceremonyID, "register")
	if err != nil {
		return err
	}
	parsed, err := protocol.ParseCredentialCreationResponseBody(body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerification, err)
	}
	o, err := p.owner(ctx)
	if err != nil {
		return err
	}
	cred, err := p.wa.CreateCredential(o, c.data, parsed)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrVerification, err)
	}
	raw, err := json.Marshal(cred)
	if err != nil {
		return err
	}
	if err := p.store.AddPasskey(ctx, cred.ID, c.name, raw); err != nil {
		return err
	}
	p.mu.Lock()
	p.count++
	p.mu.Unlock()
	return nil
}

type Device struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

func (p *Passkeys) List(ctx context.Context) ([]Device, error) {
	rows, err := p.store.ListPasskeys(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Device, len(rows))
	for i, r := range rows {
		out[i] = Device{ID: base64.RawURLEncoding.EncodeToString(r.ID), Name: r.Name, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt}
	}
	return out, nil
}

func (p *Passkeys) Delete(ctx context.Context, id string) error {
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return meta.ErrNotFound
	}
	if err := p.store.DeletePasskey(ctx, raw); err != nil {
		return err
	}
	return p.refreshCount(ctx)
}

// ResetAll 清空全部设备（紧急恢复用）。
func (p *Passkeys) ResetAll(ctx context.Context) (int64, error) {
	n, err := p.store.DeleteAllPasskeys(ctx)
	if err != nil {
		return 0, err
	}
	return n, p.refreshCount(ctx)
}

func (p *Passkeys) refreshCount(ctx context.Context) error {
	n, err := p.store.CountPasskeys(ctx)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.count = n
	p.mu.Unlock()
	return nil
}

// NewAddCode 生成一个添加新设备用的一次性验证码（需要已登录的会话才能调用）。
func (p *Passkeys) NewAddCode() (string, time.Time, error) {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		return "", time.Time{}, err
	}
	code := base32.StdEncoding.EncodeToString(b) // 8 个字符，A–Z 和 2–7
	exp := time.Now().Add(addCodeTTL)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.addCodes[code] = exp
	p.codeFails = 0
	return code, exp, nil
}

// UseAddCode 校验并作废添加码。连续输错 maxCodeFails 次后，现有添加码全部作废。
func (p *Passkeys) UseAddCode(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for c, exp := range p.addCodes {
		if now.After(exp) {
			delete(p.addCodes, c)
			continue
		}
		if subtle.ConstantTimeCompare([]byte(c), []byte(code)) == 1 {
			delete(p.addCodes, c)
			return true
		}
	}
	if p.codeFails++; p.codeFails >= maxCodeFails {
		clear(p.addCodes)
		p.codeFails = 0
	}
	return false
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
