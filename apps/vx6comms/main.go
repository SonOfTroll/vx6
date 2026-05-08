package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/vx6/vx6/internal/config"
	"github.com/vx6/vx6/internal/dht"
	"github.com/vx6/vx6/internal/identity"
	"github.com/vx6/vx6/internal/record"
	"github.com/vx6/vx6/sdk"
)

type state struct {
	mu       sync.Mutex
	client   *sdk.Client
	mode     appMode
	id       identity.Identity
	name     string
	addr     string
	cancel   context.CancelFunc
	contacts map[string]peerContact
	local    localState
	selected int32
}

func main() {
	mode := modeOpen
	if len(os.Args) > 1 && strings.TrimSpace(os.Args[1]) == "org" {
		mode = modeOrg
	}
	st := &state{
		mode:     mode,
		contacts: map[string]peerContact{},
		local: localState{
			Unread:       map[string]int{},
			SeenMessage:  map[string]bool{},
			Pending:      map[string]bool{},
			Outbox:       []queuedMessage{},
			ActiveGroups: map[string]groupRoom{},
		},
		selected: -1,
	}

	a := app.NewWithID("com.vx6.comms")
	w := a.NewWindow(windowTitle(mode))
	w.Resize(fyne.NewSize(1240, 820))

	client, err := sdk.New("")
	if err != nil {
		dialog.ShowError(err, w)
		w.ShowAndRun()
		return
	}
	st.client = client
	_ = st.loadIdentityAndConfig()

	topTitle := widget.NewLabelWithStyle("VX6 Comms", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	topSub := widget.NewLabel("Secure decentralized chat with invites, retries, ack tracking, and media transfer")
	statusLabel := widget.NewLabel("Status: idle")
	ipLabel := widget.NewLabel("IPv6: checking")
	refreshIPStatus(ipLabel)

	nameInput := widget.NewEntry()
	nameInput.SetPlaceHolder("Nickname")
	nameInput.SetText(st.name)
	emailInput := widget.NewEntry()
	emailInput.SetPlaceHolder("Email (local profile)")
	phoneInput := widget.NewEntry()
	phoneInput.SetPlaceHolder("Phone (local profile)")

	myInfo := widget.NewMultiLineEntry()
	myInfo.Disable()
	myInfo.SetMinRowsVisible(3)
	refreshMyInfo(st, myInfo)

	startBtn := widget.NewButtonWithIcon("Start Node", theme.MediaPlayIcon(), func() {
		nm := strings.TrimSpace(nameInput.Text)
		if nm == "" {
			dialog.ShowInformation("Name Required", "Please set a nickname.", w)
			return
		}
		if err := st.validateNameUnique(nm); err != nil {
			dialog.ShowError(err, w)
			return
		}
		if err := st.initAndStart(nm, emailInput.Text, phoneInput.Text); err != nil {
			dialog.ShowError(err, w)
			return
		}
		statusLabel.SetText("Status: running")
		refreshMyInfo(st, myInfo)
	})
	stopBtn := widget.NewButtonWithIcon("Stop", theme.MediaStopIcon(), func() {
		st.stopNode()
		statusLabel.SetText("Status: stopped")
	})
	renameBtn := widget.NewButton("Rename + Validate", func() {
		nm := strings.TrimSpace(nameInput.Text)
		if nm == "" {
			return
		}
		if err := st.validateNameUnique(nm); err != nil {
			dialog.ShowError(err, w)
			return
		}
		if err := st.renameLocalNode(nm); err != nil {
			dialog.ShowError(err, w)
			return
		}
		refreshMyInfo(st, myInfo)
		dialog.ShowInformation("Name Updated", "Name accepted by network check and updated locally.", w)
	})

	inviteBox := widget.NewMultiLineEntry()
	inviteBox.SetPlaceHolder("Invite link")
	inviteBox.SetMinRowsVisible(4)
	inviteBox.Wrapping = fyne.TextWrapBreak
	genInviteBtn := widget.NewButton("Generate Invite", func() {
		link, err := st.generateInvite()
		if err != nil {
			dialog.ShowError(err, w)
			return
		}
		inviteBox.SetText(link)
	})

	inviteIn := widget.NewMultiLineEntry()
	inviteIn.SetPlaceHolder("Paste invite link")
	inviteIn.SetMinRowsVisible(4)
	inviteIn.Wrapping = fyne.TextWrapBreak
	addInviteBtn := widget.NewButton("Add Contact", func() {
		if err := st.acceptInvite(inviteIn.Text); err != nil {
			dialog.ShowError(err, w)
			return
		}
		dialog.ShowInformation("Contact Added", "Request sent and contact saved.", w)
	})

	contactsList := widget.NewList(
		func() int { return len(st.sortedContacts()) },
		func() fyne.CanvasObject { return widget.NewLabel("contact") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			cs := st.sortedContacts()
			if i < 0 || i >= len(cs) {
				return
			}
			c := cs[i]
			unread := st.local.Unread[c.NodeID]
			ttl := c.NodeName
			if unread > 0 {
				ttl = fmt.Sprintf("%s (%d)", c.NodeName, unread)
			}
			o.(*widget.Label).SetText(ttl)
		},
	)

	chatLog := widget.NewMultiLineEntry()
	chatLog.Disable()
	chatLog.SetMinRowsVisible(16)
	chatInput := widget.NewMultiLineEntry()
	chatInput.SetMinRowsVisible(4)
	chatInput.SetPlaceHolder("Type message")

	sendBtn := widget.NewButton("Send", func() {
		idx := int(atomic.LoadInt32(&st.selected))
		cs := st.sortedContacts()
		if idx < 0 || idx >= len(cs) {
			dialog.ShowInformation("Select Contact", "Pick a contact from left list.", w)
			return
		}
		msg := strings.TrimSpace(chatInput.Text)
		if msg == "" {
			return
		}
		if err := st.sendMessage(cs[idx], msg); err != nil {
			dialog.ShowError(err, w)
			return
		}
		chatInput.SetText("")
		_ = st.refreshConversation(cs[idx], chatLog)
	})

	syncBtn := widget.NewButton("Sync", func() {
		if err := st.syncInboxAndRequests(w, chatLog, int(atomic.LoadInt32(&st.selected))); err != nil {
			dialog.ShowError(err, w)
			return
		}
		contactsList.Refresh()
	})

	filePath := widget.NewEntry()
	filePath.SetPlaceHolder("Path to file (video/images/docs)")
	sendFileBtn := widget.NewButton("Send File", func() {
		idx := int(atomic.LoadInt32(&st.selected))
		cs := st.sortedContacts()
		if idx < 0 || idx >= len(cs) {
			dialog.ShowInformation("Select Contact", "Pick a contact first.", w)
			return
		}
		p := strings.TrimSpace(filePath.Text)
		if p == "" {
			return
		}
		progress := dialog.NewProgress("File Transfer", "Sending...", w)
		progress.Show()
		go func() {
			err := st.sendFile(cs[idx], p, func(sent, total int64) {
				fyne.Do(func() {
					if total > 0 {
						progress.SetValue(float64(sent) / float64(total))
					}
				})
			})
			fyne.Do(func() {
				progress.Hide()
				if err != nil {
					dialog.ShowError(err, w)
					return
				}
				dialog.ShowInformation("Transfer Complete", "File sent and metadata announced in chat.", w)
			})
		}()
	})

	groupName := widget.NewEntry()
	groupName.SetPlaceHolder("Group name")
	createGroupBtn := widget.NewButton("Create Group", func() {
		if err := st.createGroup(strings.TrimSpace(groupName.Text)); err != nil {
			dialog.ShowError(err, w)
			return
		}
		groupName.SetText("")
		dialog.ShowInformation("Group Created", "Group metadata published.", w)
	})

	contactsList.OnSelected = func(id widget.ListItemID) {
		atomic.StoreInt32(&st.selected, int32(id))
		cs := st.sortedContacts()
		if id >= 0 && id < len(cs) {
			st.local.Unread[cs[id].NodeID] = 0
			_ = st.saveLocalState()
			_ = st.refreshConversation(cs[id], chatLog)
		}
	}

	leftPanel := container.NewVBox(
		widget.NewCard("Node", "", container.NewVBox(
			nameInput, emailInput, phoneInput,
			container.NewHBox(startBtn, stopBtn, renameBtn),
			statusLabel, ipLabel,
			widget.NewLabel("My Identity / Address"), myInfo,
		)),
		widget.NewCard("Contacts", "", contactsList),
	)

	centerPanel := container.NewVBox(
		widget.NewCard("Conversation", "", chatLog),
		container.NewGridWithColumns(2, sendBtn, syncBtn),
		chatInput,
	)

	rightPanel := container.NewVBox(
		widget.NewCard("Invite Link", "", container.NewVBox(genInviteBtn, inviteBox)),
		widget.NewCard("Add Contact", "", container.NewVBox(inviteIn, addInviteBtn)),
		widget.NewCard("Media", "", container.NewVBox(filePath, sendFileBtn)),
		widget.NewCard("Groups", "", container.NewVBox(groupName, createGroupBtn)),
	)

	midSplit := container.NewHSplit(centerPanel, rightPanel)
	midSplit.Offset = 0.62
	mainSplit := container.NewHSplit(leftPanel, midSplit)
	mainSplit.Offset = 0.28

	root := container.NewBorder(
		container.NewVBox(topTitle, topSub),
		container.NewHBox(layout.NewSpacer(), widget.NewLabel("VX6 Comms UI")),
		nil, nil,
		mainSplit,
	)
	w.SetContent(root)

	go func() {
		t := time.NewTicker(4 * time.Second)
		defer t.Stop()
		for range t.C {
			_ = st.syncInboxAndRequests(w, chatLog, int(atomic.LoadInt32(&st.selected)))
			_ = st.retryPending()
			contactsList.Refresh()
		}
	}()
	w.ShowAndRun()
}

func windowTitle(mode appMode) string {
	if mode == modeOrg {
		return "VX6 Comms (Org)"
	}
	return "VX6 Comms (Open)"
}

func refreshIPStatus(lbl *widget.Label) {
	v6 := false
	ifaces, _ := netInterfaceAddrs()
	for _, a := range ifaces {
		if strings.Contains(a, ":") && !strings.Contains(a, "::1") && !strings.HasPrefix(a, "fe80:") {
			v6 = true
			break
		}
	}
	if v6 {
		lbl.SetText("IPv6: available")
	} else {
		lbl.SetText("IPv6: not detected (relay fallback)")
	}
}

func netInterfaceAddrs() ([]string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, 8)
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			out = append(out, addr.String())
		}
	}
	return out, nil
}

func (s *state) initAndStart(name, email, phone string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := s.client.Init(ctx, sdk.InitOptions{Name: name, FileReceiveMode: config.FileReceiveOpen})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
	s.saveProfileMeta(email, phone)
	return s.startNode()
}

func (s *state) renameLocalNode(name string) error {
	store, err := config.NewStore(s.client.ConfigPath())
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err != nil {
		return err
	}
	cfg.Node.Name = name
	if err := store.Save(cfg); err != nil {
		return err
	}
	s.mu.Lock()
	s.name = name
	s.mu.Unlock()
	return nil
}

func (s *state) saveProfileMeta(email, phone string) {
	path := filepath.Join(filepath.Dir(s.client.ConfigPath()), "vx6comms-profile.json")
	_ = os.WriteFile(path, marshalJSON(map[string]string{"email": strings.TrimSpace(email), "phone": strings.TrimSpace(phone)}), 0o644)
}

func (s *state) startNode() error {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.mu.Unlock()
	go func() {
		_ = s.client.StartNode(ctx, os.Stdout, sdk.StartOptions{})
		s.mu.Lock()
		s.cancel = nil
		s.mu.Unlock()
	}()
	return nil
}

func (s *state) stopNode() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

func (s *state) loadIdentityAndConfig() error {
	store, err := config.NewStore(s.client.ConfigPath())
	if err != nil {
		return err
	}
	cfg, err := store.Load()
	if err == nil {
		s.name = cfg.Node.Name
		s.addr = cfg.Node.AdvertiseAddr
	}
	idStore, err := identity.NewStoreForConfig(store.Path())
	if err != nil {
		return err
	}
	id, err := idStore.Load()
	if err == nil {
		s.id = id
	}
	_ = s.loadContacts()
	_ = s.loadLocalState()
	return nil
}

func (s *state) contactsPath() string {
	return filepath.Join(filepath.Dir(s.client.ConfigPath()), "vx6comms-contacts.json")
}

func (s *state) statePath() string {
	return filepath.Join(filepath.Dir(s.client.ConfigPath()), "vx6comms-state.json")
}

func (s *state) loadContacts() error {
	data, err := os.ReadFile(s.contactsPath())
	if err != nil {
		return nil
	}
	var out map[string]peerContact
	if err := json.Unmarshal(data, &out); err != nil {
		return err
	}
	s.contacts = out
	return nil
}

func (s *state) saveContacts() error {
	return os.WriteFile(s.contactsPath(), marshalJSON(s.contacts), 0o644)
}

func (s *state) loadLocalState() error {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		return nil
	}
	var st localState
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	if st.Unread == nil {
		st.Unread = map[string]int{}
	}
	if st.SeenMessage == nil {
		st.SeenMessage = map[string]bool{}
	}
	if st.Pending == nil {
		st.Pending = map[string]bool{}
	}
	if st.ActiveGroups == nil {
		st.ActiveGroups = map[string]groupRoom{}
	}
	s.local = st
	return nil
}

func (s *state) saveLocalState() error {
	s.local.LastSyncAt = time.Now().UTC().Format(time.RFC3339)
	return os.WriteFile(s.statePath(), marshalJSON(s.local), 0o644)
}

func (s *state) sortedContacts() []peerContact {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]peerContact, 0, len(s.contacts))
	for _, c := range s.contacts {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].NodeName) < strings.ToLower(out[j].NodeName) })
	return out
}

func (s *state) generateInvite() (string, error) {
	if s.id.NodeID == "" || s.name == "" || s.addr == "" {
		if err := s.loadIdentityAndConfig(); err != nil {
			return "", err
		}
	}
	secret, err := randomSecret()
	if err != nil {
		return "", err
	}
	return inviteLink(s.id.NodeID, s.name, s.addr, secret), nil
}

func (s *state) acceptInvite(link string) error {
	req, err := parseInviteLink(link)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.contacts[req.FromID] = peerContact{
		NodeID: req.FromID, NodeName: req.FromName, Address: req.Address, Secret: req.Secret,
		AddedAt: time.Now().UTC().Format(time.RFC3339), Accepted: true, RequestID: req.RequestID,
	}
	s.mu.Unlock()
	_ = s.client.AddPeer(req.FromName, req.Address)
	_ = s.saveContacts()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	_ = s.client.DHTPut(ctx, requestKey(req.FromID), marshalJSON(friendRequest{
		RequestID: req.RequestID, FromID: s.id.NodeID, FromName: s.name, Address: s.addr, Secret: req.Secret, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}))
	return nil
}

func (s *state) sendMessage(c peerContact, text string) error {
	env, err := sealMessage(c.Secret, chatMessage{Text: text}, s.id.NodeID, c.NodeID, "msg")
	if err != nil {
		return err
	}
	if err := s.publishEnvelope(c, env); err != nil {
		s.queueMessage(c.NodeID, env, 1)
		return err
	}
	s.local.Pending[env.ID] = true
	s.queueMessage(c.NodeID, env, 1)
	_ = s.saveLocalState()
	return nil
}

func (s *state) sendFile(c peerContact, p string, onProgress func(sent, total int64)) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := s.client.SendFileWithProgress(ctx, c.Address, p, onProgress); err != nil {
		return err
	}
	meta, err := sdk.BuildSharedFile(p, "shared from vx6comms")
	if err != nil {
		return err
	}
	msg := messageEnvelope{
		ID:        "file-" + meta.ID,
		Type:      "media",
		From:      s.id.NodeID,
		To:        c.NodeID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		MediaName: meta.Name,
		MediaSize: meta.Size,
		MediaSHA:  meta.SHA256,
	}
	return s.publishEnvelope(c, msg)
}

func (s *state) publishEnvelope(c peerContact, env messageEnvelope) error {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	key := pairKey(s.id.NodeID, c.NodeID)
	var ledger conversationLedger
	if raw, err := s.client.DHTGet(ctx, key); err == nil && len(raw) > 0 {
		_ = json.Unmarshal(raw, &ledger)
	}
	if s.hasMessageID(ledger.Messages, env.ID) {
		return nil
	}
	ledger.PairKey = key
	ledger.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	ledger.Messages = append(ledger.Messages, env)
	if len(ledger.Messages) > 800 {
		ledger.Messages = ledger.Messages[len(ledger.Messages)-800:]
	}
	return s.client.DHTPut(ctx, key, marshalJSON(ledger))
}

func (s *state) refreshConversation(c peerContact, out *widget.Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	raw, err := s.client.DHTGet(ctx, pairKey(s.id.NodeID, c.NodeID))
	if err != nil {
		return nil
	}
	var ledger conversationLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return err
	}
	lines := make([]string, 0, len(ledger.Messages))
	for _, m := range ledger.Messages {
		if m.Type == "ack" {
			continue
		}
		if m.Type == "media" {
			src := "Me"
			if m.From != s.id.NodeID {
				src = c.NodeName
			}
			lines = append(lines, fmt.Sprintf("[%s] %s shared file: %s (%d bytes)", m.CreatedAt, src, m.MediaName, m.MediaSize))
			continue
		}
		msg, err := openMessage(c.Secret, m)
		if err != nil {
			continue
		}
		from := "Me"
		if m.From != s.id.NodeID {
			from = c.NodeName
		}
		lines = append(lines, fmt.Sprintf("[%s] %s: %s", m.CreatedAt, from, msg.Text))
	}
	out.SetText(strings.Join(lines, "\n"))
	return nil
}

func (s *state) syncInboxAndRequests(win fyne.Window, msgOut *widget.Entry, selected int) error {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	raw, err := s.client.DHTGet(ctx, requestKey(s.id.NodeID))
	if err == nil && len(raw) > 0 {
		var req friendRequest
		if json.Unmarshal(raw, &req) == nil && req.FromID != "" {
			s.mu.Lock()
			_, exists := s.contacts[req.FromID]
			if !exists {
				s.contacts[req.FromID] = peerContact{
					NodeID: req.FromID, NodeName: req.FromName, Address: req.Address, Secret: req.Secret,
					AddedAt: time.Now().UTC().Format(time.RFC3339), Accepted: true, RequestID: req.RequestID,
				}
				_ = s.saveContacts()
				fyne.Do(func() {
					dialog.ShowInformation("Friend Request", req.FromName+" sent a request and was added.", win)
				})
			}
			s.mu.Unlock()
		}
	}

	cs := s.sortedContacts()
	for _, c := range cs {
		_ = s.syncContactLedger(c)
	}
	if selected >= 0 && selected < len(cs) {
		_ = s.refreshConversation(cs[selected], msgOut)
	}
	_ = s.saveLocalState()
	return nil
}

func (s *state) syncContactLedger(c peerContact) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err := s.client.DHTGet(ctx, pairKey(s.id.NodeID, c.NodeID))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var ledger conversationLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return err
	}
	hasNew := false
	for _, m := range ledger.Messages {
		if m.Type == "ack" {
			delete(s.local.Pending, m.AckFor)
			continue
		}
		if s.local.SeenMessage[m.ID] {
			continue
		}
		s.local.SeenMessage[m.ID] = true
		if m.From != s.id.NodeID {
			s.local.Unread[c.NodeID] = s.local.Unread[c.NodeID] + 1
			ack := makeAckMessage(m.ID, s.id.NodeID, c.NodeID)
			_ = s.publishEnvelope(c, ack)
		}
		hasNew = true
	}
	if hasNew {
		_ = s.saveLocalState()
	}
	return nil
}

func (s *state) retryPending() error {
	now := time.Now().UTC()
	nextOut := make([]queuedMessage, 0, len(s.local.Outbox))
	for _, q := range s.local.Outbox {
		if !s.local.Pending[q.Envelope.ID] {
			continue
		}
		when, _ := time.Parse(time.RFC3339, q.NextRetry)
		if when.After(now) {
			nextOut = append(nextOut, q)
			continue
		}
		c, ok := s.contacts[q.ContactID]
		if !ok {
			continue
		}
		_ = s.publishEnvelope(c, q.Envelope)
		q.Retries++
		if q.Retries <= 5 {
			q.NextRetry = now.Add(time.Duration(4+q.Retries*2) * time.Second).Format(time.RFC3339)
			nextOut = append(nextOut, q)
		}
	}
	s.local.Outbox = nextOut
	return s.saveLocalState()
}

func (s *state) queueMessage(contactID string, env messageEnvelope, delaySeconds int) {
	for _, q := range s.local.Outbox {
		if q.Envelope.ID == env.ID {
			return
		}
	}
	s.local.Outbox = append(s.local.Outbox, queuedMessage{
		ContactID: contactID,
		Envelope:  env,
		Retries:   0,
		NextRetry: time.Now().UTC().Add(time.Duration(delaySeconds) * time.Second).Format(time.RFC3339),
	})
}

func (s *state) hasMessageID(items []messageEnvelope, id string) bool {
	for _, m := range items {
		if m.ID == id {
			return true
		}
	}
	return false
}

func (s *state) createGroup(name string) error {
	if name == "" {
		return fmt.Errorf("group name required")
	}
	secret, err := randomSecret()
	if err != nil {
		return err
	}
	id := fmt.Sprintf("grp-%d", time.Now().UnixNano())
	gr := groupRoom{
		ID: id, Name: name, Secret: secret, Members: []string{s.id.NodeID}, CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.local.ActiveGroups[id] = gr
	_ = s.saveLocalState()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	return s.client.DHTPut(ctx, groupKey(id), marshalJSON(gr))
}

func refreshMyInfo(s *state, out *widget.Entry) {
	_ = s.loadIdentityAndConfig()
	out.SetText(fmt.Sprintf("Node Name: %s\nNode ID: %s\nAddress: %s", s.name, s.id.NodeID, s.addr))
}

func (s *state) validateNameUnique(name string) error {
	if err := record.ValidateNodeName(name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Second)
	defer cancel()
	raw, err := s.client.DHTGet(ctx, dht.NodeNameKey(name))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var rec record.EndpointRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil
	}
	if rec.NodeID != "" && s.id.NodeID != "" && rec.NodeID != s.id.NodeID {
		return fmt.Errorf("name already exists in network with different node key (%s). choose another", rec.NodeID)
	}
	return nil
}
