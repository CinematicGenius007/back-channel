package main

// Wire protocol: newline-delimited JSON over TCP (TLS when the hub runs -tls/-public).
// One struct, `Msg`, is the only frame type; `t` says what it is and the other fields
// are used as documented in PROTOCOL.md. Frames are flat and additive: new fields never
// break old clients, renamed fields do. The `hello` frame is versioned (`v`), nothing else.

const (
	version      = "2.0.0"
	protoVersion = 2
	appName      = "backchannel"
	binName      = "bch"
)

// Msg is the single wire type. One JSON object per line.
type Msg struct {
	T string `json:"t"` // frame type, see PROTOCOL.md

	// envelope
	V   int    `json:"v,omitempty"`   // hello/ok: protocol version (absent = v1)
	RID string `json:"rid,omitempty"` // request id, echoed on the reply
	Ch  string `json:"ch,omitempty"`  // channel name
	CID int64  `json:"cid,omitempty"` // channel id (hub log only; clients ignore)

	// messages / files
	ID   int64   `json:"id,omitempty"`   // hub-assigned, monotonic across all channels
	TS   int64   `json:"ts,omitempty"`   // unix millis, UTC
	From string  `json:"from,omitempty"` // sender (username, guest nick, or device for clip)
	By   string  `json:"by,omitempty"`   // moderator who did it (deleted/kicked/banned/...)
	Text string  `json:"text,omitempty"` // body, reason, or status text
	Name string  `json:"name,omitempty"` // file name; cmd name; v1 hello nick
	Size int64   `json:"size,omitempty"`
	FID  string  `json:"fid,omitempty"`
	Sha  string  `json:"sha256,omitempty"`
	Hist bool    `json:"hist,omitempty"` // replayed from history
	IDs  []int64 `json:"ids,omitempty"`  // purged: message ids removed

	// hello / ok
	Token    string     `json:"token,omitempty"`    // legacy shared token (guest access, v1 hubs)
	Since    int64      `json:"since,omitempty"`    // sub (and v1 hello): replay ids > since
	Last     int64      `json:"last,omitempty"`     // synced: last id in channel
	Device   string     `json:"device,omitempty"`   // hello: client label ("mac", "win-work")
	Auth     *Auth      `json:"auth,omitempty"`     // hello v2
	Pub      string     `json:"pub,omitempty"`      // hello: this device's X25519 identity public key (base64)
	User     string     `json:"user,omitempty"`     // ok: your username
	Role     string     `json:"role,omitempty"`     // ok: your server role; role event: new channel role
	Session  string     `json:"session,omitempty"`  // ok: session token (only after password/invite login)
	Channels []ChanInfo `json:"channels,omitempty"` // ok / res(channels, create, join)
	Limits   *LimitInfo `json:"limits,omitempty"`   // ok
	Users    []string   `json:"users,omitempty"`    // who / v1 ok
	Until    int64      `json:"until,omitempty"`    // muted/banned: unix millis

	// end-to-end encryption (see ENCRYPTION.md and e2e.go)
	Epoch    int    `json:"epoch,omitempty"`     // msg/file: the key epoch it was sealed with · keyreq/keyshare: which epoch · settings: channel's current epoch
	ToUser   string `json:"to_user,omitempty"`   // keyshare: recipient account
	ToDevice string `json:"to_device,omitempty"` // keyshare: recipient device label
	FromUser string `json:"from_user,omitempty"` // keyreq (hub-filled): requester's account
	FromDev  string `json:"from_dev,omitempty"`  // keyreq (hub-filled): requester's device label
	FromPub  string `json:"from_pub,omitempty"`  // keyreq/keyshare: sender's device public key (base64)
	Wrapped  string `json:"wrapped,omitempty"`   // keyshare: base64(nonce||ciphertext) wrapping the channel key
	Peers    []Peer `json:"peers,omitempty"`     // res: e2epeers

	// cmd / res / err
	Code  string            `json:"code,omitempty"`  // err/res: machine-readable error code
	OK    bool              `json:"ok,omitempty"`    // res
	Args  map[string]string `json:"args,omitempty"`  // cmd
	Lines []string          `json:"lines,omitempty"` // res: tabular output
}

// Peer is one currently-connected device of a channel member, for key distribution.
type Peer struct {
	User   string `json:"user"`
	Device string `json:"device"`
	Pub    string `json:"pub"`
}

// Auth is the v2 hello credential. Exactly one of the three forms is used:
//
//	{session}                     resume a saved session
//	{user, pass}                  password login → server returns a new session
//	{invite, user, pass}          redeem an invite that allows sign-up → creates the account
//	{user, pass, register:true}   open registration (only if the hub allows it)
type Auth struct {
	Session  string `json:"session,omitempty"`
	User     string `json:"user,omitempty"`
	Pass     string `json:"pass,omitempty"`
	Invite   string `json:"invite,omitempty"`
	Register bool   `json:"register,omitempty"`
}

// ChanInfo describes one channel as the receiving user sees it.
type ChanInfo struct {
	Name     string `json:"name"`
	Topic    string `json:"topic,omitempty"`
	Role     string `json:"role,omitempty"` // your role in it ("" = server admin visiting)
	Unread   int    `json:"unread,omitempty"`
	Last     int64  `json:"last,omitempty"` // last message id
	Readonly bool   `json:"readonly,omitempty"`
	Members  int    `json:"members,omitempty"`
	Default  bool   `json:"default,omitempty"`
	Expire   int64  `json:"expire,omitempty"` // seconds; messages older than this self-destruct
	E2E      bool   `json:"e2e,omitempty"`    // end-to-end encrypted: hub only ever sees ciphertext
	Epoch    int    `json:"epoch,omitempty"`  // e2e: current key epoch (0 if not e2e)
}

// LimitInfo tells a client what the hub will accept from it.
type LimitInfo struct {
	MaxMsg    int   `json:"max_msg"`              // bytes
	MaxFile   int64 `json:"max_file"`             // bytes
	QuotaLeft int64 `json:"quota_left,omitempty"` // bytes; 0 = unlimited/not applicable
}

// Error codes. Anywhere a response would differ based on secret state (does this
// channel exist? is that a real user?) the same code AND text must be returned.
const (
	codeAuth      = "auth"      // bad credentials / session expired
	codeBanned    = "banned"    // server-wide ban
	codeNoChan    = "nochan"    // no such channel OR not a member — identical on purpose
	codePerm      = "perm"      // you lack the role
	codeRateLimit = "ratelimit" // slow down
	codeTooLong   = "toolong"   // message too large
	codeMuted     = "muted"
	codeReadonly  = "readonly"
	codeBadArg    = "badarg"
	codeLimit     = "limit"  // quota / max users / max channels
	codeExists    = "exists" // name taken
	codeNotFound  = "notfound"
	codeGuest     = "guest" // guests can't do that
)

const textNoChan = "no such channel"
