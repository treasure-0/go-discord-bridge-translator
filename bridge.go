package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
)

const (
	bridgeHookName = "Bridge-Translator"
	maxUploadBytes = 9 << 20 // stay under Discord's default 10 MiB upload limit
	snippetLen     = 80
)

// Route says where messages from a channel go and into which language.
type Route struct {
	Target string
	Lang   Lang // language of the target channel
	Local  Lang // main language of the source channel
}

// Bridge mirrors messages between paired channels, translating them on the way.
type Bridge struct {
	s      *discordgo.Session
	tr     Translator
	store  *Store
	routes map[string]Route
	http   *http.Client

	ctx     context.Context
	cancel  context.CancelFunc
	queues  map[string]chan func()
	wg      sync.WaitGroup
	closing atomic.Bool
	queueMu sync.RWMutex

	hooksMu sync.Mutex
	hooks   map[string]*discordgo.Webhook // target channel -> webhook

	notifier *Notifier // optional alerts to the owner
}

func NewBridge(s *discordgo.Session, tr Translator, store *Store, routes map[string]Route) *Bridge {
	ctx, cancel := context.WithCancel(context.Background())
	b := &Bridge{
		s: s, tr: tr, store: store, routes: routes,
		http:   &http.Client{Timeout: 60 * time.Second},
		ctx:    ctx,
		cancel: cancel,
		queues: map[string]chan func(){},
		hooks:  map[string]*discordgo.Webhook{},
	}
	// One worker per source channel: messages are translated in parallel across
	// channels but always posted in their original order within a channel.
	for ch := range routes {
		q := make(chan func(), 256)
		b.queues[ch] = q
		b.wg.Add(1)
		go func() {
			defer b.wg.Done()
			for job := range q {
				b.safely(job)
			}
		}()
	}
	return b
}

func (b *Bridge) Register() {
	b.s.AddHandler(b.onCreate)
	b.s.AddHandler(b.onUpdate)
	b.s.AddHandler(b.onDelete)
}

// Close stops accepting events and waits (briefly) for queued work to finish.
func (b *Bridge) Close() {
	b.queueMu.Lock()
	b.closing.Store(true)
	for _, q := range b.queues {
		close(q)
	}
	b.queueMu.Unlock()

	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		log.Println("shutdown: gave up waiting for queued messages")
	}
	b.cancel()
}

func (b *Bridge) enqueue(channelID string, job func()) {
	b.queueMu.RLock()
	defer b.queueMu.RUnlock()
	q, ok := b.queues[channelID]
	if !ok || b.closing.Load() {
		return
	}
	select {
	case q <- job:
	default:
		recordError("очередь канала %s переполнена, событие пропущено", channelID)
	}
}

func (b *Bridge) safely(job func()) {
	defer func() {
		if r := recover(); r != nil {
			recordError("паника в обработчике: %v", r)
		}
	}()
	job()
}

// fromHuman filters out bots and webhooks (including our own translations) to avoid loops.
func fromHuman(m *discordgo.Message) bool {
	return m != nil && m.Author != nil && !m.Author.Bot && m.WebhookID == ""
}

// ---------------------------------------------------------------------------
// Event handlers
// ---------------------------------------------------------------------------

func (b *Bridge) onCreate(_ *discordgo.Session, e *discordgo.MessageCreate) {
	m := e.Message
	if !fromHuman(m) || (m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply) {
		return
	}
	if _, ok := b.routes[m.ChannelID]; ok {
		b.enqueue(m.ChannelID, func() { b.mirror(m) })
	}
}

func (b *Bridge) onUpdate(_ *discordgo.Session, e *discordgo.MessageUpdate) {
	m := e.Message
	if m == nil || m.WebhookID != "" || (m.Author != nil && m.Author.Bot) {
		return
	}
	if _, ok := b.routes[m.ChannelID]; ok {
		b.enqueue(m.ChannelID, func() { b.edit(m) })
	}
}

func (b *Bridge) onDelete(_ *discordgo.Session, e *discordgo.MessageDelete) {
	if _, ok := b.routes[e.ChannelID]; ok {
		id := e.ID
		b.enqueue(e.ChannelID, func() { b.remove(id) })
	}
}

// ---------------------------------------------------------------------------
// New message
// ---------------------------------------------------------------------------

func (b *Bridge) mirror(m *discordgo.Message) {
	route := b.routes[m.ChannelID]
	start := time.Now()

	content, atts, stickers := m.Content, m.Attachments, m.StickerItems
	prefix := b.replyPrefix(m, route)
	// Forwarded messages carry their content in a snapshot.
	if len(m.MessageSnapshots) > 0 && m.MessageSnapshots[0].Message != nil {
		fw := m.MessageSnapshots[0].Message
		content, atts, stickers = fw.Content, fw.Attachments, fw.StickerItems
		prefix = forwardedLabel(route.Lang)
	}

	res, provider := b.translate(content, replyHint(m), route.Lang)
	text := res.Text
	files, links := b.downloadAttachments(atts, stickers)

	body := strings.TrimSpace(strings.Join(append([]string{text}, links...), "\n"))
	if body == "" && prefix == "" && len(files) == 0 {
		return // nothing we can mirror (e.g. a poll or embed-only message)
	}

	hook, err := b.webhook(route.Target)
	if err != nil {
		recordError("вебхук для канала %s: %v", route.Target, err)
		return
	}

	chunks := splitMessage(prefix, body, discordLimit)
	var sent []string
	for i, chunk := range chunks {
		params := &discordgo.WebhookParams{
			Content:         chunk,
			Username:        webhookName(displayName(m)),
			AvatarURL:       avatarURL(m),
			AllowedMentions: &discordgo.MessageAllowedMentions{}, // never ping anyone from a translation
		}
		if i == len(chunks)-1 {
			params.Files = files.discordFiles()
		}
		msg, err := b.execute(route.Target, &hook, params, files, i == len(chunks)-1)
		if err != nil {
			recordError("отправка в канал %s: %v", route.Target, err)
			break
		}
		sent = append(sent, msg.ID)
	}
	if len(sent) == 0 {
		return
	}

	b.store.Put(Record{
		SourceID:      m.ID,
		SourceChannel: m.ChannelID,
		TargetChannel: route.Target,
		TargetIDs:     sent,
		WebhookID:     hook.ID,
		Prefix:        prefix,
		Author:        displayName(m),
		SourceText:    snippet(content, snippetLen),
		TargetText:    snippet(text, snippetLen),
		ContentHash:   contentHash(m.Content),
		LocalID:       b.postLocalCopy(m, content, res.Lang, route),
	})
	ops.Add("bridge", "messages", 1)
	switch provider {
	case "passthrough":
		ops.Add("bridge", "passthrough", 1)
	case "none":
		ops.Add("bridge", "untranslated", 1)
	}
	log.Printf("mirrored %s -> %s via %s in %s", m.ID, route.Target, provider, time.Since(start).Round(time.Millisecond))
}

// execute posts via the cached webhook, recreating it once if it was deleted.
func (b *Bridge) execute(channelID string, hook **discordgo.Webhook, p *discordgo.WebhookParams, files attachments, withFiles bool) (*discordgo.Message, error) {
	msg, err := b.s.WebhookExecute((*hook).ID, (*hook).Token, true, p)
	if err == nil || !isUnknownWebhook(err) {
		return msg, err
	}
	b.forgetWebhook(channelID)
	h, herr := b.webhook(channelID)
	if herr != nil {
		return nil, herr
	}
	*hook = h
	if withFiles {
		p.Files = files.discordFiles() // readers were consumed by the failed attempt
	}
	return b.s.WebhookExecute(h.ID, h.Token, true, p)
}

// translate returns the translated text and which provider produced it.
// If every provider fails, the original text is posted with a small notice
// so the conversation isn't lost.
func (b *Bridge) translate(text, hint string, to Lang) (Result, string) {
	if !needsTranslation(text) {
		return Result{Text: text}, "passthrough"
	}
	var out Result
	var provider string
	var err error
	if named, ok := b.tr.(interface {
		TranslateNamed(context.Context, string, string, Lang) (Result, string, error)
	}); ok {
		out, provider, err = named.TranslateNamed(b.ctx, text, hint, to)
	} else {
		out, err = b.tr.Translate(b.ctx, text, hint, to)
		provider = b.tr.Name()
	}
	if err != nil {
		recordError("все переводчики не ответили: %v", err)
		b.notifier.Alert("all-failed", 15*time.Minute,
			"🚨 Ни один переводчик не ответил, сообщения пересылаются без перевода. Ошибки: "+truncate(err.Error(), 1500))
		return Result{Text: text + "\n-# ⚠️ translation unavailable"}, "none"
	}
	return out, provider
}

func forwardedLabel(to Lang) string {
	if to == LangRU {
		return "-# ↪ *переслано*\n"
	}
	return "-# ↪ *forwarded*\n"
}

func replyHint(m *discordgo.Message) string {
	if m.ReferencedMessage != nil {
		return truncate(m.ReferencedMessage.Content, 500)
	}
	return ""
}

// replyPrefix renders the quote line for replies. Webhooks can't create real
// Discord replies, so we link to the counterpart of the replied-to message in
// the target channel instead.
func (b *Bridge) replyPrefix(m *discordgo.Message, route Route) string {
	ref := m.ReferencedMessage
	if m.MessageReference == nil || m.MessageReference.Type != discordgo.MessageReferenceTypeDefault || ref == nil {
		return ""
	}
	// Replying to an original message (or to the bot's local copy of it) -> point at its translation.
	refID := ref.ID
	if rec, ok := b.store.ByLocal(ref.ID); ok {
		refID = rec.SourceID
	}
	if rec, ok := b.store.BySource(refID); ok && rec.TargetChannel == route.Target {
		return replyQuote(rec.Author, rec.TargetText, messageLink(m.GuildID, rec.TargetChannel, rec.TargetIDs[0]))
	}
	// Replying to a translation -> point at the original it came from.
	if rec, ok := b.store.ByTarget(ref.ID); ok && rec.SourceChannel == route.Target {
		return replyQuote(rec.Author, rec.SourceText, messageLink(m.GuildID, rec.SourceChannel, rec.SourceID))
	}
	// Unknown (older than our history): link to the message itself.
	name := "?"
	if ref.Author != nil {
		name = ref.Author.DisplayName()
	}
	return replyQuote(name, snippet(ref.Content, snippetLen), messageLink(m.GuildID, ref.ChannelID, ref.ID))
}

// ---------------------------------------------------------------------------
// Edits and deletions
// ---------------------------------------------------------------------------

func (b *Bridge) edit(m *discordgo.Message) {
	rec, ok := b.store.BySource(m.ID)
	if !ok || contentHash(m.Content) == rec.ContentHash || (m.Content == "" && len(m.Attachments) == 0) {
		return // not ours, an embed/preview update, or a partial event
	}
	hook, err := b.webhook(rec.TargetChannel)
	if err != nil || hook.ID != rec.WebhookID {
		recordError("правка %s: вебхук недоступен", m.ID)
		return
	}

	route := b.routes[m.ChannelID]
	res, _ := b.translate(m.Content, replyHint(m), route.Lang)
	text := res.Text
	chunks := splitMessage(rec.Prefix, text, discordLimit)

	// Keep the same number of messages: merge overflow into the last one,
	// delete leftovers if the new text is shorter.
	n := len(rec.TargetIDs)
	if len(chunks) > n {
		last := strings.Join(chunks[n-1:], "\n")
		chunks = append(chunks[:n-1], truncate(last, discordLimit))
	}
	var kept []string
	for i, id := range rec.TargetIDs {
		if i >= len(chunks) {
			if err := b.s.WebhookMessageDelete(hook.ID, hook.Token, id); err != nil {
				recordError("правка %s: удаление лишней части: %v", m.ID, err)
			}
			continue
		}
		content := chunks[i]
		if _, err := b.s.WebhookMessageEdit(hook.ID, hook.Token, id, &discordgo.WebhookEdit{
			Content:         &content,
			AllowedMentions: &discordgo.MessageAllowedMentions{},
		}); err != nil {
			recordError("правка %s: %v", m.ID, err)
		}
		kept = append(kept, id)
	}

	rec.TargetIDs = kept
	rec.SourceText = snippet(m.Content, snippetLen)
	rec.TargetText = snippet(text, snippetLen)
	rec.LocalID = b.updateLocalCopy(m, rec.LocalID, res.Lang, route)
	rec.ContentHash = contentHash(m.Content)
	b.store.Put(rec)
}

func (b *Bridge) remove(sourceID string) {
	rec, ok := b.store.BySource(sourceID)
	if !ok {
		return
	}
	hook, err := b.webhook(rec.TargetChannel)
	if err == nil && hook.ID == rec.WebhookID {
		for _, id := range rec.TargetIDs {
			if err := b.s.WebhookMessageDelete(hook.ID, hook.Token, id); err != nil {
				recordError("удаление %s: %v", id, err)
			}
		}
	}
	if rec.LocalID != "" {
		if err := b.s.ChannelMessageDelete(rec.SourceChannel, rec.LocalID); err != nil {
			recordError("удаление локального перевода %s: %v", rec.LocalID, err)
		}
	}
	b.store.Delete(sourceID)
}

// ---------------------------------------------------------------------------
// Webhooks
// ---------------------------------------------------------------------------

func (b *Bridge) webhook(channelID string) (*discordgo.Webhook, error) {
	b.hooksMu.Lock()
	defer b.hooksMu.Unlock()
	if h, ok := b.hooks[channelID]; ok {
		return h, nil
	}
	hooks, err := b.s.ChannelWebhooks(channelID)
	if err != nil {
		return nil, fmt.Errorf("list webhooks (does the bot have Manage Webhooks?): %w", err)
	}
	for _, h := range hooks {
		if h.Name == bridgeHookName && h.Token != "" {
			b.hooks[channelID] = h
			return h, nil
		}
	}
	h, err := b.s.WebhookCreate(channelID, bridgeHookName, "")
	if err != nil {
		b.notifier.Alert("webhook-"+channelID, time.Hour,
			"🚨 Не получается создать вебхук в канале <#"+channelID+">. Проверь, что у бота есть право Manage Webhooks.")
		return nil, fmt.Errorf("create webhook: %w", err)
	}
	b.hooks[channelID] = h
	return h, nil
}

func (b *Bridge) forgetWebhook(channelID string) {
	b.hooksMu.Lock()
	delete(b.hooks, channelID)
	b.hooksMu.Unlock()
}

func isUnknownWebhook(err error) bool {
	var rest *discordgo.RESTError
	return errors.As(err, &rest) && rest.Message != nil && rest.Message.Code == discordgo.ErrCodeUnknownWebhook
}

// ---------------------------------------------------------------------------
// Author identity
// ---------------------------------------------------------------------------

// displayName prefers the server nickname, then the global display name.
func displayName(m *discordgo.Message) string {
	if m.Member != nil && m.Member.Nick != "" {
		return m.Member.Nick
	}
	return m.Author.DisplayName()
}

// avatarURL prefers the per-server avatar when the member has one.
func avatarURL(m *discordgo.Message) string {
	if m.Member != nil && m.Member.Avatar != "" && m.GuildID != "" {
		if strings.HasPrefix(m.Member.Avatar, "a_") {
			return discordgo.EndpointGuildMemberAvatarAnimated(m.GuildID, m.Author.ID, m.Member.Avatar)
		}
		return discordgo.EndpointGuildMemberAvatar(m.GuildID, m.Author.ID, m.Member.Avatar)
	}
	return m.Author.AvatarURL("128")
}

// ---------------------------------------------------------------------------
// Attachments and stickers
// ---------------------------------------------------------------------------

type attachment struct {
	name, contentType string
	data              []byte
}

type attachments []attachment

func (a attachments) discordFiles() []*discordgo.File {
	files := make([]*discordgo.File, len(a))
	for i, f := range a {
		files[i] = &discordgo.File{Name: f.name, ContentType: f.contentType, Reader: bytes.NewReader(f.data)}
	}
	return files
}

// downloadAttachments re-uploads files (Discord CDN links expire, so linking
// isn't enough). Files that don't fit the upload limit are posted as links.
// Stickers can't be sent by webhooks, so they become image links.
func (b *Bridge) downloadAttachments(atts []*discordgo.MessageAttachment, stickers []*discordgo.StickerItem) (attachments, []string) {
	var files attachments
	var links []string
	budget := maxUploadBytes
	for _, a := range atts {
		if a.Size > budget {
			links = append(links, a.URL)
			continue
		}
		data, err := b.fetch(a.URL, budget)
		if err != nil {
			recordError("вложение %s: %v", a.Filename, err)
			links = append(links, a.URL)
			continue
		}
		budget -= len(data)
		files = append(files, attachment{name: a.Filename, contentType: a.ContentType, data: data})
	}
	for _, st := range stickers {
		switch st.FormatType {
		case discordgo.StickerFormatTypeLottie:
			links = append(links, "*["+st.Name+"]*")
		case discordgo.StickerFormatTypeGIF:
			links = append(links, "https://media.discordapp.net/stickers/"+st.ID+".gif")
		default:
			links = append(links, "https://media.discordapp.net/stickers/"+st.ID+".png?size=160")
		}
	}
	return files, links
}

func (b *Bridge) fetch(url string, limit int) ([]byte, error) {
	req, err := http.NewRequestWithContext(b.ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := b.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("too large")
	}
	return data, nil
}

// ---------------------------------------------------------------------------
// Local copies for foreign languages
// ---------------------------------------------------------------------------

// needsLocalCopy: the message is in a language other than the channel's main
// one and long enough to be worth translating locally ("ok", "gg", "danke" are not).
func needsLocalCopy(content, lang string, route Route) bool {
	if lang == "" || lang == "und" || lang == route.Local.ISO {
		return false
	}
	plain := protectedRe.ReplaceAllString(content, " ")
	letters := 0
	for _, r := range plain {
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return len(strings.Fields(plain)) >= 3 || letters >= 20
}

func localCopyText(lang string, route Route, translation string) string {
	head := "🌐 **" + strings.ToUpper(lang) + " → " + strings.ToUpper(route.Local.ISO) + ":** "
	return truncate(head+translation, discordLimit)
}

// postLocalCopy replies to a foreign-language message in its own channel with a
// translation into the channel's main language. The reply comes from the bot
// itself, so the bridge ignores it and it never crosses to the other channel.
func (b *Bridge) postLocalCopy(m *discordgo.Message, content, lang string, route Route) string {
	if !needsLocalCopy(content, lang, route) {
		return ""
	}
	res, provider := b.translate(content, replyHint(m), route.Local)
	if provider == "none" || provider == "passthrough" {
		return ""
	}
	msg, err := b.s.ChannelMessageSendComplex(m.ChannelID, &discordgo.MessageSend{
		Content:         localCopyText(lang, route, res.Text),
		Reference:       &discordgo.MessageReference{MessageID: m.ID, ChannelID: m.ChannelID, GuildID: m.GuildID},
		AllowedMentions: &discordgo.MessageAllowedMentions{}, // no pings, not even the author
	})
	if err != nil {
		recordError("локальный перевод в канале %s: %v", m.ChannelID, err)
		return ""
	}
	ops.Add("bridge", "localcopies", 1)
	return msg.ID
}

// updateLocalCopy keeps the local copy in sync after an edit and returns its ID.
func (b *Bridge) updateLocalCopy(m *discordgo.Message, localID, lang string, route Route) string {
	if localID == "" {
		return b.postLocalCopy(m, m.Content, lang, route)
	}
	if !needsLocalCopy(m.Content, lang, route) {
		if err := b.s.ChannelMessageDelete(m.ChannelID, localID); err != nil {
			recordError("удаление локального перевода %s: %v", localID, err)
		}
		return ""
	}
	res, provider := b.translate(m.Content, replyHint(m), route.Local)
	if provider == "none" {
		return localID
	}
	if _, err := b.s.ChannelMessageEdit(m.ChannelID, localID, localCopyText(lang, route, res.Text)); err != nil {
		recordError("правка локального перевода %s: %v", localID, err)
	}
	return localID
}
