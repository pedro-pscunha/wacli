package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/openclaw/wacli/internal/fsutil"
	"github.com/openclaw/wacli/internal/out"
	"github.com/openclaw/wacli/internal/store"
	"github.com/openclaw/wacli/internal/wa"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

type WAClient interface {
	Close()
	Disconnect()
	IsAuthed() bool
	IsConnected() bool
	SetAutoReconnect(enabled bool) (previous bool, ok bool)
	Connect(ctx context.Context, opts wa.ConnectOptions) error

	AddEventHandler(handler func(any)) uint32
	RemoveEventHandler(id uint32)
	ReconnectWithBackoff(ctx context.Context, minDelay, maxDelay time.Duration, opts wa.ConnectOptions) error

	ResolveChatName(ctx context.Context, chat types.JID, pushName string) string
	ResolveLIDToPN(ctx context.Context, jid types.JID) types.JID
	ResolvePNToLID(ctx context.Context, jid types.JID) types.JID
	GetUserInfo(ctx context.Context, jids []types.JID) (map[types.JID]types.UserInfo, error)
	IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
	GetContact(ctx context.Context, jid types.JID) (types.ContactInfo, error)
	GetAllContacts(ctx context.Context) (map[types.JID]types.ContactInfo, error)

	GetJoinedGroups(ctx context.Context) ([]*types.GroupInfo, error)
	GetGroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
	CreateGroup(ctx context.Context, req wa.CreateGroupRequest) (*types.GroupInfo, error)
	SetGroupName(ctx context.Context, jid types.JID, name string) error
	SetGroupTopic(ctx context.Context, jid types.JID, topic string) error
	SetGroupAnnounce(ctx context.Context, jid types.JID, announce bool) error
	SetGroupLocked(ctx context.Context, jid types.JID, locked bool) error
	UpdateGroupParticipants(ctx context.Context, group types.JID, users []types.JID, action wa.GroupParticipantAction) ([]types.GroupParticipant, error)
	GetGroupRequestParticipants(ctx context.Context, group types.JID) ([]types.GroupParticipantRequest, error)
	UpdateGroupRequestParticipants(ctx context.Context, group types.JID, users []types.JID, action wa.GroupParticipantRequestAction) ([]types.GroupParticipant, error)
	GetGroupInviteLink(ctx context.Context, group types.JID, reset bool) (string, error)
	JoinGroupWithLink(ctx context.Context, code string) (types.JID, error)
	LeaveGroup(ctx context.Context, group types.JID) error

	GetNewsletterInfoWithInvite(ctx context.Context, key string) (*types.NewsletterMetadata, error)
	FollowNewsletter(ctx context.Context, jid types.JID) error
	UnfollowNewsletter(ctx context.Context, jid types.JID) error
	GetSubscribedNewsletters(ctx context.Context) ([]*types.NewsletterMetadata, error)
	GetNewsletterInfo(ctx context.Context, jid types.JID) (*types.NewsletterMetadata, error)

	SendText(ctx context.Context, to types.JID, text string) (types.MessageID, error)
	SendProtoMessage(ctx context.Context, to types.JID, msg *waProto.Message) (types.MessageID, error)
	SendProtoMessageWithExtra(ctx context.Context, to types.JID, msg *waProto.Message, mediaHandle string) (types.MessageID, error)
	SendReaction(ctx context.Context, chat, sender types.JID, targetID types.MessageID, reaction string) (types.MessageID, error)
	SendPoll(ctx context.Context, to types.JID, name string, options []string, selectable int, ephemeral bool) (types.MessageID, error)
	SendPollVote(ctx context.Context, pollInfo *types.MessageInfo, options []string) (types.MessageID, error)
	DecryptPollVote(ctx context.Context, evt *events.Message) (*waE2E.PollVoteMessage, error)
	DecryptSecretEncryptedMessage(ctx context.Context, evt *events.Message) (*waE2E.Message, error)
	RevokeMessage(ctx context.Context, chat types.JID, targetID types.MessageID) (types.MessageID, error)
	DeleteMessageForMe(ctx context.Context, info types.MessageInfo, deleteMedia bool) error
	EditMessage(ctx context.Context, chat types.JID, targetID types.MessageID, text string) (types.MessageID, error)
	ArchiveChat(ctx context.Context, target types.JID, archive bool, lastMsgTS time.Time, lastMsgKey *waCommon.MessageKey, beforeApply func()) ([]any, error)
	PinChat(ctx context.Context, target types.JID, pin bool, beforeApply func()) ([]any, error)
	MuteChat(ctx context.Context, target types.JID, mute bool, duration time.Duration, beforeApply func()) ([]any, error)
	MarkChatAsRead(ctx context.Context, target types.JID, read bool, lastMsgTS time.Time, lastMsgKey *waCommon.MessageKey, beforeApply func()) ([]any, error)
	Upload(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	UploadNewsletter(ctx context.Context, data []byte, mediaType whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	DownloadMediaToFile(ctx context.Context, directPath string, encFileHash, fileHash, mediaKey []byte, fileLength uint64, mediaType, mmsType string, targetPath string) (int64, error)
	DownloadRetriedMediaToFile(ctx context.Context, directPath string, fileHash, mediaKey []byte, fileLength uint64, mediaType, targetPath string) (int64, error)
	SendMediaRetryReceipt(ctx context.Context, info *types.MessageInfo, mediaKey []byte) error
	MarkRead(ctx context.Context, ids []types.MessageID, timestamp time.Time, chat, sender types.JID, addressing types.AddressingMode) (types.ReceiptType, error)

	SendChatPresence(ctx context.Context, jid types.JID, state types.ChatPresence, media types.ChatPresenceMedia) error
	SendPresence(ctx context.Context, presence types.Presence) error
	SubscribePresence(ctx context.Context, jid types.JID) error
	ParseWebMessage(chatJID types.JID, webMsg *waWeb.WebMessageInfo) (*events.Message, error)
	DecryptReaction(ctx context.Context, reaction *events.Message) (*waProto.ReactionMessage, error)
	SetManualHistorySyncDownload(enabled bool)
	DownloadHistorySync(ctx context.Context, notif *waE2E.HistorySyncNotification) (*waHistorySync.HistorySync, error)
	DeleteHistorySyncMedia(ctx context.Context, notif *waE2E.HistorySyncNotification) error
	RequestHistorySyncOnDemand(ctx context.Context, lastKnown types.MessageInfo, count int) (types.MessageID, error)
	FetchAppState(ctx context.Context, name string, fullSync, onlyIfNotSynced bool) error
	FetchAppStateEvents(ctx context.Context, name string, fullSync, onlyIfNotSynced bool) ([]any, error)
	FetchAppStateSnapshotUnverified(ctx context.Context, name string) ([]appstate.Mutation, error)
	EditLabel(ctx context.Context, labelID, name string, color int32, deleted bool) error
	LabelChat(ctx context.Context, target types.JID, labelID string, labeled bool) error
	RequestAppStateRecovery(ctx context.Context, name string) (types.MessageID, error)
	HasLocalAppState(ctx context.Context, name string) (bool, error)
	Logout(ctx context.Context) error
	LinkedJID() string
	LinkedLID() string
	OwnDevices(ctx context.Context) ([]wa.OwnDevice, error)

	SetProfilePicture(ctx context.Context, avatar []byte) (string, error)
	GetProfilePictureInfo(ctx context.Context, jid types.JID, preview bool, existingID string) (*types.ProfilePictureInfo, error)
	SetStatusMessage(ctx context.Context, msg string) error
	SetProfileName(ctx context.Context, name string) error
	GetBusinessProfile(ctx context.Context, jid types.JID) (*types.BusinessProfile, error)

	StarMessage(ctx context.Context, info types.MessageInfo, starred bool) error
	PinInChat(ctx context.Context, chat, sender types.JID, fromMe bool, id types.MessageID, pin bool, duration time.Duration) (types.MessageID, error)
	UpdateBlocklist(ctx context.Context, jid types.JID, block bool) (*types.Blocklist, error)
	GetBlocklist(ctx context.Context) (*types.Blocklist, error)
	SetGroupPhoto(ctx context.Context, jid types.JID, avatar []byte) (string, error)
	SetGroupJoinApprovalMode(ctx context.Context, jid types.JID, on bool) error
	SetGroupMemberAddMode(ctx context.Context, jid types.JID, allMembers bool) error
	SetDisappearingTimer(ctx context.Context, chat types.JID, timer time.Duration) error
	SetDefaultDisappearingTimer(ctx context.Context, timer time.Duration) error
	NewsletterToggleMute(ctx context.Context, jid types.JID, mute bool) error
	CreateNewsletter(ctx context.Context, name, description string) (*types.NewsletterMetadata, error)
	DeleteChat(ctx context.Context, chat types.JID, lastMsgTS time.Time, deleteMedia bool) error
	GetPrivacySettings(ctx context.Context) (types.PrivacySettings, error)
	SetPrivacySetting(ctx context.Context, name types.PrivacySettingType, value types.PrivacySetting) (types.PrivacySettings, error)
	RejectCall(ctx context.Context, from types.JID, callID string) error
}

// ErrNoClient is returned by OpenWA when the app was built with NoClient.
var ErrNoClient = errors.New("this command works on the local store only and may not open the WhatsApp session")

// ErrDelegateOnly is returned by Connect when the app is delegate-only.
var ErrDelegateOnly = errors.New("daemon not running; refusing to connect outside the sync daemon (--delegate-only)")

type Options struct {
	StoreDir      string
	Version       string
	JSON          bool
	Events        *out.EventWriter
	AllowUnauthed bool
	ReadOnly      bool
	// DelegateOnly refuses Connect: only the sync daemon may hold a session.
	DelegateOnly bool
	// NoClient makes OpenWA refuse: the App works on the local store alone and must never build a WhatsApp
	// client, not even an unconnected one. Set for kinds that only read or write wacli.db.
	NoClient bool
	// WA, when set, is used instead of opening the session store. Tests inject
	// a fake client here; production leaves it nil.
	WA WAClient
}

type App struct {
	opts                    Options
	waMu                    sync.Mutex
	wa                      WAClient
	sessionState            *sessionObservation
	sessionHandler          uint32
	connectGate             chan struct{}
	sessionResolver         *readOnlySessionResolver
	db                      *store.DB
	statusMu                sync.Mutex
	status                  *syncStatus
	chatStateSync           chan struct{}
	appStatePersist         appStatePersistenceSequencer
	appStateRecoveryWorkers sync.WaitGroup
	appStateRecoveryMu      sync.Mutex
	appStateRecoveryClosing bool
	appStateRecoveryRetryAt map[string]time.Time
	// Background continuations of recovery attempts the primary answered late,
	// at most one per collection. Guarded by appStateRecoveryMu.
	lateRecoveries   map[string]*lateAppStateRecovery
	lateRecoveryStop chan struct{}
	manualFetchMu    sync.Mutex
	manualFetches    map[string]int
	// Degraded app-state mode: collections whose last full sync is known not
	// to verify (mirrored durably in wacli.db and rehydrated once per
	// process), and the time of the last background recovery request per
	// collection. Guarded by appStateDegradedMu.
	appStateDegradedMu        sync.Mutex
	appStateFullSyncLoaded    bool
	appStateFullSyncBroken    map[string]bool
	appStateRecoveryRequested map[string]time.Time
	heartbeatLast             atomic.Int64
	groupInfo                 groupInfoCache
	presenceWatch             presenceWatch
	// liveSync is the running Sync's storage context, so work done on its
	// connection (a live history backfill) stores messages as the sync does.
	liveSync atomic.Pointer[liveSyncState]
	// backfillTails holds, per chat, the cleanup of the last backfill runner that
	// is still listening for late replies. A new runner for the same chat ends it
	// first, so a late reply is never taken by two runners.
	backfillTailMu sync.Mutex
	backfillTails  map[string]*backfillTail
}

// backfillTail is one finished backfill runner still listening for late replies.
type backfillTail struct {
	end func()
}

// liveSyncState is what a request served by the sync daemon needs to store
// history the way the sync itself does: the daemon's options, its storage
// limits and its media queue.
type liveSyncState struct {
	opts         SyncOptions
	limits       *syncStorageLimits
	enqueueMedia func(chatJID, msgID string)
}

func New(opts Options) (*App, error) {
	if opts.StoreDir == "" {
		return nil, fmt.Errorf("store dir is required")
	}

	indexPath := filepath.Join(opts.StoreDir, "wacli.db")

	var (
		db  *store.DB
		err error
	)
	if opts.ReadOnly {
		db, err = store.OpenReadOnly(indexPath)
	} else {
		if err := fsutil.EnsurePrivateDir(opts.StoreDir); err != nil {
			return nil, fmt.Errorf("create store dir: %w", err)
		}
		db, err = store.Open(indexPath)
	}
	if err != nil {
		return nil, err
	}

	chatStateSync := make(chan struct{}, 1)
	chatStateSync <- struct{}{}
	return &App{opts: opts, db: db, chatStateSync: chatStateSync}, nil
}

func (a *App) OpenWA() error {
	a.waMu.Lock()
	defer a.waMu.Unlock()
	if a.opts.NoClient {
		return ErrNoClient
	}
	if a.opts.ReadOnly {
		if a.wa != nil {
			return nil
		}
		return fmt.Errorf("read-only mode: command would open the WhatsApp session store")
	}
	if a.wa == nil && a.opts.WA != nil {
		a.wa = a.opts.WA
	}
	if a.wa == nil {
		sessionPath := filepath.Join(a.opts.StoreDir, "session.db")
		cli, err := wa.New(wa.Options{StorePath: sessionPath, KeyStateStore: a.db})
		if err != nil {
			return err
		}
		a.wa = cli
	}
	if a.sessionState == nil {
		state := newSessionObservation(a.opts.StoreDir)
		a.sessionState = state
		a.sessionHandler = a.wa.AddEventHandler(func(evt any) { a.observeSessionState(state, evt) })
		a.connectGate = make(chan struct{}, 1)
		a.connectGate <- struct{}{}
	}
	return nil
}

func (a *App) Close() {
	a.appStateRecoveryMu.Lock()
	a.appStateRecoveryClosing = true
	if a.lateRecoveryStop != nil {
		close(a.lateRecoveryStop)
		a.lateRecoveryStop = nil
	}
	a.appStateRecoveryMu.Unlock()
	a.waMu.Lock()
	waClient := a.wa
	sessionResolver := a.sessionResolver
	sessionState, sessionHandler := a.sessionState, a.sessionHandler
	a.waMu.Unlock()
	if waClient != nil {
		waClient.Disconnect()
		if sessionState != nil {
			waClient.RemoveEventHandler(sessionHandler)
		}
	}
	// A completed command frontier may hand later ready tasks to a background
	// drainer. Keep SQLite open until that drainer has finished every write.
	a.appStateRecoveryWorkers.Wait()
	_ = a.appStatePersist.waitIdle(context.Background())
	if sessionResolver != nil {
		_ = sessionResolver.Close()
	}
	if waClient != nil {
		waClient.Close()
	}
	if a.db != nil {
		_ = a.db.Close()
	}
}

func (a *App) EnsureAuthed(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Every caller connects next. Refuse first so the error names the cause,
	// not a missing session on a store that only the daemon opens.
	if a.opts.DelegateOnly {
		return ErrDelegateOnly
	}
	if err := a.OpenWA(); err != nil {
		return err
	}
	if a.wa.IsAuthed() {
		if a.opts.ReadOnly {
			return nil
		}
		return a.migrateHistoricalLIDs(ctx)
	}
	return fmt.Errorf("not authenticated; run `wacli auth`")
}

func (a *App) WA() WAClient {
	a.waMu.Lock()
	defer a.waMu.Unlock()
	return a.wa
}

type LocalResolver interface {
	ResolveChatName(context.Context, types.JID, string) string
	ResolveLIDToPN(context.Context, types.JID) types.JID
	ResolvePNToLID(context.Context, types.JID) types.JID
}

func (a *App) LocalResolver() (LocalResolver, error) {
	if !a.opts.ReadOnly {
		if err := a.OpenWA(); err != nil {
			return nil, err
		}
		return a.WA(), nil
	}
	return a.ReadOnlyResolver()
}

// ReadOnlyResolver reads session identities without opening or upgrading a client store.
func (a *App) ReadOnlyResolver() (LocalResolver, error) {
	a.waMu.Lock()
	defer a.waMu.Unlock()
	if a.sessionResolver != nil {
		return a.sessionResolver, nil
	}
	resolver, err := openReadOnlySessionResolver(filepath.Join(a.opts.StoreDir, "session.db"))
	if err != nil {
		return nil, err
	}
	a.sessionResolver = resolver
	return resolver, nil
}

func (a *App) DB() *store.DB { return a.db }
func (a *App) Events() *out.EventWriter {
	return a.opts.Events
}
func (a *App) StoreDir() string    { return a.opts.StoreDir }
func (a *App) Version() string     { return a.opts.Version }
func (a *App) AllowUnauthed() bool { return a.opts.AllowUnauthed }
func (a *App) ReadOnly() bool      { return a.opts.ReadOnly }

func (a *App) Connect(ctx context.Context, allowQR bool, qrWriter func(string)) error {
	if a.opts.ReadOnly {
		return fmt.Errorf("read-only mode: command would connect to WhatsApp")
	}
	if a.opts.DelegateOnly {
		return ErrDelegateOnly
	}
	if err := a.OpenWA(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.connectGate:
	}
	defer func() { a.connectGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.sessionState.prepareConnect(a.wa.IsConnected); err != nil {
		return err
	}
	if err := a.wa.Connect(ctx, wa.ConnectOptions{
		AllowQR:  allowQR,
		OnQRCode: qrWriter,
	}); err != nil {
		return err
	}
	return a.sessionState.waitForLogin(ctx)
}
