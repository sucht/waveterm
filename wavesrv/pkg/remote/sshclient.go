// Copyright 2023-2024, Command Line Inc.
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kevinburke/ssh_config"
	"github.com/wavetermdev/waveterm/waveshell/pkg/base"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/scbus"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/sstore"
	"github.com/wavetermdev/waveterm/wavesrv/pkg/userinput"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type UserInputCancelError struct {
	Err error
}

func (uice UserInputCancelError) Error() string {
	return uice.Err.Error()
}

// This exists to trick the ssh library into continuing to try
// different public keys even when the current key cannot be
// properly parsed
func createDummySigner() ([]ssh.Signer, error) {
	dummyKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	dummySigner, err := ssh.NewSignerFromKey(dummyKey)
	if err != nil {
		return nil, err
	}
	return []ssh.Signer{dummySigner}, nil

}

// This is a workaround to only process one identity file at a time,
// even if they have passphrases. It must be combined with retryable
// authentication to work properly
//
// Despite returning an array of signers, we only ever provide one since
// it allows proper user interaction in between attempts
//
// A significant number of errors end up returning dummy values as if
// they were successes. An error in this function prevents any other
// keys from being attempted. But if there's an error because of a dummy
// file, the library can still try again with a new key.
func createPublicKeyCallback(sshKeywords *SshKeywords, passphrase string) func() ([]ssh.Signer, error) {
	var identityFiles []string
	existingKeys := make(map[string][]byte)

	// checking the file early prevents us from needing to send a
	// dummy signer if there's a problem with the signer
	for _, identityFile := range sshKeywords.IdentityFile {
		privateKey, err := os.ReadFile(base.ExpandHomeDir(identityFile))
		if err != nil {
			// skip this key and try with the next
			continue
		}
		existingKeys[identityFile] = privateKey
		identityFiles = append(identityFiles, identityFile)
	}
	// require pointer to modify list in closure
	identityFilesPtr := &identityFiles

	return func() ([]ssh.Signer, error) {
		if len(*identityFilesPtr) == 0 {
			return nil, fmt.Errorf("no identity files remaining")
		}
		identityFile := (*identityFilesPtr)[0]
		*identityFilesPtr = (*identityFilesPtr)[1:]
		privateKey, ok := existingKeys[identityFile]
		if !ok {
			log.Printf("error with existingKeys, this should never happen")
			// skip this key and try with the next
			return createDummySigner()
		}
		signer, err := ssh.ParsePrivateKey(privateKey)
		if err == nil {
			return []ssh.Signer{signer}, err
		}
		if _, ok := err.(*ssh.PassphraseMissingError); !ok {
			// skip this key and try with the next
			return createDummySigner()
		}

		signer, err = ssh.ParsePrivateKeyWithPassphrase(privateKey, []byte(passphrase))
		if err == nil {
			return []ssh.Signer{signer}, err
		}
		if err != x509.IncorrectPasswordError && err.Error() != "bcrypt_pbkdf: empty password" {
			// skip this key and try with the next
			return createDummySigner()
		}

		// batch mode deactivates user input
		if sshKeywords.BatchMode {
			// skip this key and try with the next
			return createDummySigner()
		}

		request := &userinput.UserInputRequestType{
			ResponseType: "text",
			QueryText:    fmt.Sprintf("Enter passphrase for the SSH key: %s", identityFile),
			Title:        "Publickey Auth + Passphrase",
		}
		ctx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		response, err := userinput.GetUserInput(ctx, scbus.MainRpcBus, request)
		if err != nil {
			// this is an error where we actually do want to stop
			// trying keys
			return nil, UserInputCancelError{Err: err}
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(privateKey, []byte(response.Text))
		if err != nil {
			// skip this key and try with the next
			return createDummySigner()
		}
		return []ssh.Signer{signer}, err
	}
}

func createDefaultPasswordCallbackPrompt(password string) func() (secret string, err error) {
	return func() (secret string, err error) {
		// this should be modified to return an error if no password is stored
		// but an empty password is not sufficient because some systems allow
		// empty passwords
		return password, nil
	}
}

func createInteractivePasswordCallbackPrompt(remoteDisplayName string) func() (secret string, err error) {
	return func() (secret string, err error) {
		// limited to 15 seconds for some reason. this should be investigated more
		// in the future
		ctx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		queryText := fmt.Sprintf(
			"Password Authentication requested from connection  \n"+
				"%s\n\n"+
				"Password:", remoteDisplayName)
		request := &userinput.UserInputRequestType{
			ResponseType: "text",
			QueryText:    queryText,
			Markdown:     true,
			Title:        "Password Authentication",
		}
		response, err := userinput.GetUserInput(ctx, scbus.MainRpcBus, request)
		if err != nil {
			return "", err
		}
		return response.Text, nil
	}
}

func createCombinedPasswordCallbackPrompt(password string, remoteDisplayName string) func() (secret string, err error) {
	var once sync.Once
	return func() (secret string, err error) {
		var prompt func() (secret string, err error)
		once.Do(func() { prompt = createDefaultPasswordCallbackPrompt(password) })
		if prompt == nil {
			prompt = createInteractivePasswordCallbackPrompt(remoteDisplayName)
		}
		return prompt()
	}
}

func createNaiveKbdInteractiveChallenge(password string) func(name, instruction string, questions []string, echos []bool) (answers []string, err error) {
	return func(name, instruction string, questions []string, echos []bool) (answers []string, err error) {
		for _, q := range questions {
			if strings.Contains(strings.ToLower(q), "password") {
				answers = append(answers, password)
			} else {
				answers = append(answers, "")
			}
		}
		return answers, nil
	}
}

func createInteractiveKbdInteractiveChallenge(remoteName string) func(name, instruction string, questions []string, echos []bool) (answers []string, err error) {
	return func(name, instruction string, questions []string, echos []bool) (answers []string, err error) {
		if len(questions) != len(echos) {
			return nil, fmt.Errorf("bad response from server: questions has len %d, echos has len %d", len(questions), len(echos))
		}
		for i, question := range questions {
			echo := echos[i]
			answer, err := promptChallengeQuestion(question, echo, remoteName)
			if err != nil {
				return nil, err
			}
			answers = append(answers, answer)
		}
		return answers, nil
	}
}

func promptChallengeQuestion(question string, echo bool, remoteName string) (answer string, err error) {
	// limited to 15 seconds for some reason. this should be investigated more
	// in the future
	ctx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancelFn()
	queryText := fmt.Sprintf(
		"Keyboard Interactive Authentication requested from connection  \n"+
			"%s\n\n"+
			"%s", remoteName, question)
	request := &userinput.UserInputRequestType{
		ResponseType: "text",
		QueryText:    queryText,
		Markdown:     true,
		Title:        "Keyboard Interactive Authentication",
	}
	response, err := userinput.GetUserInput(ctx, scbus.MainRpcBus, request)
	if err != nil {
		return "", err
	}
	return response.Text, nil
}

func createCombinedKbdInteractiveChallenge(password string, remoteName string) ssh.KeyboardInteractiveChallenge {
	var once sync.Once
	return func(name, instruction string, questions []string, echos []bool) (answers []string, err error) {
		var challenge ssh.KeyboardInteractiveChallenge
		once.Do(func() { challenge = createNaiveKbdInteractiveChallenge(password) })
		if challenge == nil {
			challenge = createInteractiveKbdInteractiveChallenge(remoteName)
		}
		return challenge(name, instruction, questions, echos)
	}
}

func openKnownHostsForEdit(knownHostsFilename string) (*os.File, error) {
	path, _ := filepath.Split(knownHostsFilename)
	err := os.MkdirAll(path, 0700)
	if err != nil {
		return nil, err
	}
	return os.OpenFile(knownHostsFilename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
}

func writeToKnownHosts(knownHostsFile string, newLine string, getUserVerification func() (*userinput.UserInputResponsePacketType, error)) error {
	if getUserVerification == nil {
		getUserVerification = func() (*userinput.UserInputResponsePacketType, error) {
			return &userinput.UserInputResponsePacketType{
				Type:    "confirm",
				Confirm: true,
			}, nil
		}
	}

	path, _ := filepath.Split(knownHostsFile)
	err := os.MkdirAll(path, 0700)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(knownHostsFile, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	// do not close writeable files with defer

	// this file works, so let's ask the user for permission
	response, err := getUserVerification()
	if err != nil {
		f.Close()
		return UserInputCancelError{Err: err}
	}
	if !response.Confirm {
		f.Close()
		return UserInputCancelError{Err: fmt.Errorf("canceled by the user")}
	}

	_, err = f.WriteString(newLine)
	if err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func createUnknownKeyVerifier(knownHostsFile string, hostname string, remote string, key ssh.PublicKey) func() (*userinput.UserInputResponsePacketType, error) {
	base64Key := base64.StdEncoding.EncodeToString(key.Marshal())
	queryText := fmt.Sprintf(
		"The authenticity of host '%s (%s)' can't be established "+
			"as it **does not exist in any checked known_hosts files**. "+
			"The host you are attempting to connect to provides this %s key:  \n"+
			"%s.\n\n"+
			"**Would you like to continue connecting?** If so, the key will be permanently "+
			"added to the file %s "+
			"to protect from future man-in-the-middle attacks.", hostname, remote, key.Type(), base64Key, knownHostsFile)
	request := &userinput.UserInputRequestType{
		ResponseType: "confirm",
		QueryText:    queryText,
		Markdown:     true,
		Title:        "Known Hosts Key Missing",
	}
	return func() (*userinput.UserInputResponsePacketType, error) {
		ctx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		return userinput.GetUserInput(ctx, scbus.MainRpcBus, request)
	}
}

func createMissingKnownHostsVerifier(knownHostsFile string, hostname string, remote string, key ssh.PublicKey) func() (*userinput.UserInputResponsePacketType, error) {
	base64Key := base64.StdEncoding.EncodeToString(key.Marshal())
	queryText := fmt.Sprintf(
		"The authenticity of host '%s (%s)' can't be established "+
			"as **no known_hosts files could be found**. "+
			"The host you are attempting to connect to provides this %s key:  \n"+
			"%s.\n\n"+
			"**Would you like to continue connecting?** If so:  \n"+
			"- %s will be created  \n"+
			"- the key will be added to %s\n\n"+
			"This will protect from future man-in-the-middle attacks.", hostname, remote, key.Type(), base64Key, knownHostsFile, knownHostsFile)
	request := &userinput.UserInputRequestType{
		ResponseType: "confirm",
		QueryText:    queryText,
		Markdown:     true,
		Title:        "Known Hosts File Missing",
	}
	return func() (*userinput.UserInputResponsePacketType, error) {
		ctx, cancelFn := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancelFn()
		return userinput.GetUserInput(ctx, scbus.MainRpcBus, request)
	}
}

func lineContainsMatch(line []byte, matches [][]byte) bool {
	for _, match := range matches {
		if bytes.Contains(line, match) {
			return true
		}
	}
	return false
}

func createHostKeyCallback(opts *sstore.SSHOpts) (ssh.HostKeyCallback, error) {
	rawUserKnownHostsFiles, _ := ssh_config.GetStrict(opts.SSHHost, "UserKnownHostsFile")
	userKnownHostsFiles := strings.Fields(rawUserKnownHostsFiles) // TODO - smarter splitting escaped spaces and quotes
	rawGlobalKnownHostsFiles, _ := ssh_config.GetStrict(opts.SSHHost, "GlobalKnownHostsFile")
	globalKnownHostsFiles := strings.Fields(rawGlobalKnownHostsFiles) // TODO - smarter splitting escaped spaces and quotes

	osUser, err := user.Current()
	if err != nil {
		return nil, err
	}
	var unexpandedKnownHostsFiles []string
	if osUser.Username == "root" {
		unexpandedKnownHostsFiles = globalKnownHostsFiles
	} else {
		unexpandedKnownHostsFiles = append(userKnownHostsFiles, globalKnownHostsFiles...)
	}

	var knownHostsFiles []string
	for _, filename := range unexpandedKnownHostsFiles {
		knownHostsFiles = append(knownHostsFiles, base.ExpandHomeDir(filename))
	}

	// there are no good known hosts files
	if len(knownHostsFiles) == 0 {
		return nil, fmt.Errorf("no known_hosts files provided by ssh. defaults are overridden")
	}

	var unreadableFiles []string

	// the library we use isn't very forgiving about files that are formatted
	// incorrectly. if a problem file is found, it is removed from our list
	// and we try again
	var basicCallback ssh.HostKeyCallback
	for basicCallback == nil && len(knownHostsFiles) > 0 {
		var err error
		basicCallback, err = knownhosts.New(knownHostsFiles...)
		if serr, ok := err.(*os.PathError); ok {
			badFile := serr.Path
			unreadableFiles = append(unreadableFiles, badFile)
			var okFiles []string
			for _, filename := range knownHostsFiles {
				if filename != badFile {
					okFiles = append(okFiles, filename)
				}
			}
			if len(okFiles) >= len(knownHostsFiles) {
				return nil, fmt.Errorf("problem file (%s) doesn't exist. this should not be possible", badFile)
			}
			knownHostsFiles = okFiles
		} else if err != nil {
			// TODO handle obscure problems if possible
			return nil, fmt.Errorf("known_hosts formatting error: %+v", err)
		}
	}

	waveHostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		err := basicCallback(hostname, remote, key)
		if err == nil {
			// success
			return nil
		} else if _, ok := err.(*knownhosts.RevokedError); ok {
			// revoked credentials are refused outright
			return err
		} else if _, ok := err.(*knownhosts.KeyError); !ok {
			// this is an unknown error (note the !ok is opposite of usual)
			return err
		}
		serr, _ := err.(*knownhosts.KeyError)
		if len(serr.Want) == 0 {
			// the key was not found

			// try to write to a file that could be parsed
			var err error
			for _, filename := range knownHostsFiles {
				newLine := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
				getUserVerification := createUnknownKeyVerifier(filename, hostname, remote.String(), key)
				err = writeToKnownHosts(filename, newLine, getUserVerification)
				if err == nil {
					break
				}
				if serr, ok := err.(UserInputCancelError); ok {
					return serr
				}
			}

			// try to write to a file that could not be read (file likely doesn't exist)
			// should catch cases where there is no known_hosts file
			if err != nil {
				for _, filename := range unreadableFiles {
					newLine := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
					getUserVerification := createMissingKnownHostsVerifier(filename, hostname, remote.String(), key)
					err = writeToKnownHosts(filename, newLine, getUserVerification)
					if err == nil {
						knownHostsFiles = []string{filename}
						break
					}
					if serr, ok := err.(UserInputCancelError); ok {
						return serr
					}
				}
			}
			if err != nil {
				return err
			}
		} else {
			// the key changed
			correctKeyFingerprint := base64.StdEncoding.EncodeToString(key.Marshal())
			var bulletListKnownHosts []string
			for _, knownHostName := range knownHostsFiles {
				withBulletPoint := "- " + knownHostName
				bulletListKnownHosts = append(bulletListKnownHosts, withBulletPoint)
			}
			var offendingKeysFmt []string
			for _, badKey := range serr.Want {
				formattedKey := "- " + base64.StdEncoding.EncodeToString(badKey.Key.Marshal())
				offendingKeysFmt = append(offendingKeysFmt, formattedKey)
			}
			alertText := fmt.Sprintf("**WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!**\n\n"+
				"If this is not expected, it is possible that someone could be trying to "+
				"eavesdrop on you via a man-in-the-middle attack. "+
				"Alternatively, the host you are connecting to may have changed its key. "+
				"The %s key sent by the remote hist has the fingerprint:  \n"+
				"%s\n\n"+
				"If you are sure this is correct, please update your known_hosts files to "+
				"remove the lines with the offending before trying to connect again.  \n"+
				"**Known Hosts Files**  \n"+
				"%s\n\n"+
				"**Offending Keys**  \n"+
				"%s", key.Type(), correctKeyFingerprint, strings.Join(bulletListKnownHosts, "  \n"), strings.Join(offendingKeysFmt, "  \n"))
			update := scbus.MakeUpdatePacket()
			update.AddUpdate(sstore.AlertMessageType{
				Markdown: true,
				Title:    "Known Hosts Key Changed",
				Message:  alertText,
			})
			scbus.MainUpdateBus.DoUpdate(update)
			return fmt.Errorf("remote host identification has changed")
		}

		updatedCallback, err := knownhosts.New(knownHostsFiles...)
		if err != nil {
			return err
		}
		// try one final time
		return updatedCallback(hostname, remote, key)
	}

	return waveHostKeyCallback, nil
}

func ConnectToClient(opts *sstore.SSHOpts, remoteDisplayName string) (*ssh.Client, error) {
	sshConfigKeywords, err := findSshConfigKeywords(opts.SSHHost)
	if err != nil {
		return nil, err
	}

	sshKeywords, err := combineSshKeywords(opts, sshConfigKeywords)
	if err != nil {
		return nil, err
	}

	publicKeyCallback := ssh.PublicKeysCallback(createPublicKeyCallback(sshKeywords, opts.SSHPassword))
	keyboardInteractive := ssh.KeyboardInteractive(createCombinedKbdInteractiveChallenge(opts.SSHPassword, remoteDisplayName))
	passwordCallback := ssh.PasswordCallback(createCombinedPasswordCallbackPrompt(opts.SSHPassword, remoteDisplayName))

	// batch mode turns off interactive input. this means the number of
	// attemtps must drop to 1 with this setup
	var attemptsAllowed int
	if sshKeywords.BatchMode {
		attemptsAllowed = 1
	} else {
		attemptsAllowed = 2
	}

	// exclude gssapi-with-mic and hostbased until implemented
	authMethodMap := map[string]ssh.AuthMethod{
		"publickey":            ssh.RetryableAuthMethod(publicKeyCallback, len(sshKeywords.IdentityFile)),
		"keyboard-interactive": ssh.RetryableAuthMethod(keyboardInteractive, attemptsAllowed),
		"password":             ssh.RetryableAuthMethod(passwordCallback, attemptsAllowed),
	}

	authMethodActiveMap := map[string]bool{
		"publickey":            sshKeywords.PubkeyAuthentication,
		"keyboard-interactive": sshKeywords.KbdInteractiveAuthentication,
		"password":             sshKeywords.PasswordAuthentication,
	}

	var authMethods []ssh.AuthMethod
	for _, authMethodName := range sshKeywords.PreferredAuthentications {
		authMethodActive, ok := authMethodActiveMap[authMethodName]
		if !ok || !authMethodActive {
			continue
		}
		authMethod, ok := authMethodMap[authMethodName]
		if !ok {
			continue
		}
		authMethods = append(authMethods, authMethod)
	}

	hostKeyCallback, err := createHostKeyCallback(opts)
	if err != nil {
		return nil, err
	}

	clientConfig := &ssh.ClientConfig{
		User:            sshKeywords.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
	}
	networkAddr := sshKeywords.HostName + ":" + sshKeywords.Port
	return ssh.Dial("tcp", networkAddr, clientConfig)
}

type SshKeywords struct {
	User                         string
	HostName                     string
	Port                         string
	IdentityFile                 []string
	BatchMode                    bool
	PubkeyAuthentication         bool
	PasswordAuthentication       bool
	KbdInteractiveAuthentication bool
	PreferredAuthentications     []string
}

func combineSshKeywords(opts *sstore.SSHOpts, configKeywords *SshKeywords) (*SshKeywords, error) {
	sshKeywords := &SshKeywords{}

	if opts.SSHUser != "" {
		sshKeywords.User = opts.SSHUser
	} else if configKeywords.User != "" {
		sshKeywords.User = configKeywords.User
	} else {
		user, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("failed to get user for ssh: %+v", err)
		}
		sshKeywords.User = user.Username
	}

	// we have to check the host value because of the weird way
	// we store the pattern as the hostname for imported remotes
	if configKeywords.HostName != "" {
		sshKeywords.HostName = configKeywords.HostName
	} else {
		sshKeywords.HostName = opts.SSHHost
	}

	if opts.SSHPort != 0 && opts.SSHPort != 22 {
		sshKeywords.Port = strconv.Itoa(opts.SSHPort)
	} else if configKeywords.Port != "" && configKeywords.Port != "22" {
		sshKeywords.Port = configKeywords.Port
	} else {
		sshKeywords.Port = "22"
	}

	sshKeywords.IdentityFile = []string{opts.SSHIdentity}
	sshKeywords.IdentityFile = append(sshKeywords.IdentityFile, configKeywords.IdentityFile...)

	// these are not officially supported in the waveterm frontend but can be configured
	// in ssh config files
	sshKeywords.BatchMode = configKeywords.BatchMode
	sshKeywords.PubkeyAuthentication = configKeywords.PubkeyAuthentication
	sshKeywords.PasswordAuthentication = configKeywords.PasswordAuthentication
	sshKeywords.KbdInteractiveAuthentication = configKeywords.KbdInteractiveAuthentication
	sshKeywords.PreferredAuthentications = configKeywords.PreferredAuthentications

	return sshKeywords, nil
}

// note that a `var == "yes"` will default to false
// but `var != "no"` will default to true
// when given unexpected strings
func findSshConfigKeywords(hostPattern string) (*SshKeywords, error) {
	ssh_config.ReloadConfigs()
	sshKeywords := &SshKeywords{}
	var err error

	sshKeywords.User, err = ssh_config.GetStrict(hostPattern, "User")
	if err != nil {
		return nil, err
	}

	sshKeywords.HostName, err = ssh_config.GetStrict(hostPattern, "HostName")
	if err != nil {
		return nil, err
	}

	sshKeywords.Port, err = ssh_config.GetStrict(hostPattern, "Port")
	if err != nil {
		return nil, err
	}

	sshKeywords.IdentityFile = ssh_config.GetAll(hostPattern, "IdentityFile")

	batchModeRaw, err := ssh_config.GetStrict(hostPattern, "BatchMode")
	if err != nil {
		return nil, err
	}
	sshKeywords.BatchMode = (strings.ToLower(batchModeRaw) == "yes")

	// we currently do not support host-bound or unbound but will use yes when they are selected
	pubkeyAuthenticationRaw, err := ssh_config.GetStrict(hostPattern, "PubkeyAuthentication")
	if err != nil {
		return nil, err
	}
	sshKeywords.PubkeyAuthentication = (strings.ToLower(pubkeyAuthenticationRaw) != "no")

	passwordAuthenticationRaw, err := ssh_config.GetStrict(hostPattern, "PasswordAuthentication")
	if err != nil {
		return nil, err
	}
	sshKeywords.PasswordAuthentication = (strings.ToLower(passwordAuthenticationRaw) != "no")

	kbdInteractiveAuthenticationRaw, err := ssh_config.GetStrict(hostPattern, "KbdInteractiveAuthentication")
	if err != nil {
		return nil, err
	}
	sshKeywords.KbdInteractiveAuthentication = (strings.ToLower(kbdInteractiveAuthenticationRaw) != "no")

	// these are parsed as a single string and must be separated
	// these are case sensitive in openssh so they are here too
	preferredAuthenticationsRaw, err := ssh_config.GetStrict(hostPattern, "PreferredAuthentications")
	if err != nil {
		return nil, err
	}
	sshKeywords.PreferredAuthentications = strings.Split(preferredAuthenticationsRaw, ",")

	return sshKeywords, nil
}
