package mailer

import (
	"context"
	"fmt"
	"io"
	"net/textproto"
	"time"

	"github.com/gophish/gophish/config"

	"github.com/gophish/gomail"
	log "github.com/gophish/gophish/logger"
	"github.com/sirupsen/logrus"
)

// MaxReconnectAttempts is the maximum number of times we should reconnect to a server
var MaxReconnectAttempts = 10

// ErrMaxConnectAttempts is thrown when the maximum number of reconnect attempts
// is reached.
type ErrMaxConnectAttempts struct {
	underlyingError error
}

// Error returns the wrapped error response
func (e *ErrMaxConnectAttempts) Error() string {
	errString := "Max connection attempts exceeded"
	if e.underlyingError != nil {
		errString = fmt.Sprintf("%s - %s", errString, e.underlyingError.Error())
	}
	return errString
}

// Mailer is an interface that defines an object used to queue and
// send mailer.Mail instances.
type Mailer interface {
	Start(ctx context.Context)
	Queue([]Mail)
	SetSender([]config.Sender)
	SetBatchSize(int)
	SetMinSize(int)
	SetBatchWait(time.Duration)
}

// Sender exposes the common operations required for sending email.
type Sender interface {
	Send(from string, to []string, msg io.WriterTo) error
	Close() error
	Reset() error
}

// Dialer dials to an SMTP server and returns the SendCloser
type Dialer interface {
	Dial() (Sender, error)
}

// Mail is an interface that handles the common operations for email messages
type Mail interface {
	Backoff(reason error) error
	Error(err error) error
	Success() error
	Generate(msg *gomail.Message) error
	GetDialer() (Dialer, error)
	GetSmtpFrom() (string, error)
}

type SMTPHelper interface {
	GetDialerBySender(sender config.Sender) (Dialer, error)
	GetSmtpFrom(sender config.Sender) (string, error)
}

type SMTPEntry struct {
	dialer Dialer
	from   string
}

// MailWorker is the worker that receives slices of emails
// on a channel to send. It's assumed that every slice of emails received is meant
// to be sent to the same server.
type MailWorker struct {
	queue      chan []Mail
	senders    []config.Sender
	batchSize  int
	minSize    int
	batchWait  time.Duration
	smtpHelper SMTPHelper
}

type MailOption func(worker *MailWorker)

// NewMailWorker returns an instance of MailWorker with the mail queue
// initialized.
func NewMailWorker(opts ...MailOption) *MailWorker {
	w := &MailWorker{
		queue: make(chan []Mail),
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

func WithSender(senders []config.Sender) MailOption {
	return func(w *MailWorker) {
		w.senders = senders
	}
}

func WithBatchSize(size int) MailOption {
	return func(w *MailWorker) {
		w.batchSize = size
	}
}

func WithMinSize(num int) MailOption {
	return func(w *MailWorker) {
		w.minSize = num
	}
}

func WithBatchWait(wait time.Duration) MailOption {
	return func(w *MailWorker) {
		w.batchWait = wait
	}
}

func WithSTMPHelper(helper SMTPHelper) MailOption {
	return func(w *MailWorker) {
		w.smtpHelper = helper
	}
}

func (mw *MailWorker) SetSender(sender []config.Sender) {
	mw.senders = sender
}

func (mw *MailWorker) SetBatchSize(size int) {
	mw.batchSize = size
}

func (mw *MailWorker) SetMinSize(num int) {
	mw.minSize = num
}

func (mw *MailWorker) SetBatchWait(wait time.Duration) {
	mw.batchWait = wait
}

func (mw *MailWorker) SetSMTPHelper(helper SMTPHelper) {
	mw.smtpHelper = helper
}

// Start launches the mail worker to begin listening on the Queue channel
// for new slices of Mail instances to process.
func (mw *MailWorker) Start(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ms := <-mw.queue:
			var dialerArr []SMTPEntry
			if len(mw.senders) > 0 {
				for _, senderConf := range mw.senders {
					dialer, err := mw.smtpHelper.GetDialerBySender(senderConf)
					if err != nil {
						log.Error(err)
						continue
					}
					sender, err := dialHost(ctx, dialer)
					if err != nil {
						log.Error(err)
						continue
					}
					sender.Close()
					smtpFrom, err := mw.smtpHelper.GetSmtpFrom(senderConf)
					if err != nil {
						log.Error(err)
						continue
					}
					dialerArr = append(dialerArr, SMTPEntry{dialer: dialer, from: smtpFrom})
				}
			}
			if len(dialerArr) > 0 {
				// 开启配置：使用配置中提供的所有邮箱分组发送
				mw.sendMailMultiSender(ctx, dialerArr, ms)
			} else {
				// 原逻辑：一次演练任务使用配置的一个邮箱发送
				go func(ctx context.Context, ms []Mail) {
					dialer, err := ms[0].GetDialer()
					if err != nil {
						errorMail(err, ms)
						return
					}
					smtpFrom, err := ms[0].GetSmtpFrom()
					if err != nil {
						errorMail(err, ms)
						return
					}
					mw.sendMail(ctx, dialer, smtpFrom, ms)
				}(ctx, ms)
			}

		}
	}
}

// Queue sends the provided mail to the internal queue for processing.
func (mw *MailWorker) Queue(ms []Mail) {
	mw.queue <- ms
}

// errorMail is a helper to handle erroring out a slice of Mail instances
// in the case that an unrecoverable error occurs.
func errorMail(err error, ms []Mail) {
	for _, m := range ms {
		m.Error(err)
	}
}

// dialHost attempts to make a connection to the host specified by the Dialer.
// It returns MaxReconnectAttempts if the number of connection attempts has been
// exceeded.
func dialHost(ctx context.Context, dialer Dialer) (Sender, error) {
	sendAttempt := 0
	var sender Sender
	var err error
	for {
		select {
		case <-ctx.Done():
			return nil, nil
		default:
			break
		}
		sender, err = dialer.Dial()
		if err == nil {
			break
		}
		sendAttempt++
		if sendAttempt == MaxReconnectAttempts {
			err = &ErrMaxConnectAttempts{
				underlyingError: err,
			}
			break
		}
	}
	return sender, err
}

func splitMail(ms []Mail, n int, min int) [][]Mail {
	groups := make([][]Mail, 0)
	length := len(ms)
	// 总数小于单组最小数量，只需要一组
	if length < min {
		return [][]Mail{ms}
	}
	// 总数小于所有组最小数量的和，优先放满前面的组
	if length < n*min {
		index := 0
		for i := 0; i < n; i++ {
			if index >= length {
				break
			}
			if index+min >= length {
				groups = append(groups, ms[index:])
				break
			}
			groups = append(groups, ms[index:index+min])
			index += min
		}
		return groups
	}
	// 平均分到各组
	batchSize := length / n
	start := 0
	for i := 1; i <= n; i++ {
		if i != n {
			groups = append(groups, ms[start:start+batchSize])
			start += batchSize
		} else {
			groups = append(groups, ms[start:])
		}
	}
	return groups
}

// 使用多个邮箱进行分组发送
func (mw *MailWorker) sendMailMultiSender(ctx context.Context, senders []SMTPEntry, ms []Mail) {
	mailGroup := splitMail(ms, len(senders), mw.minSize)
	for i, _ := range mailGroup {
		go mw.sendMail(ctx, senders[i].dialer, senders[i].from, mailGroup[i])
	}
}

// sendMail attempts to send the provided Mail instances.
// If the context is cancelled before all of the mail are sent,
// sendMail just returns and does not modify those emails.
// 每发送batchSize条邮件，休眠wait
func (mw *MailWorker) sendMail(ctx context.Context, dialer Dialer, smtpFrom string, ms []Mail) {
	sender, err := dialHost(ctx, dialer)
	if err != nil {
		log.Warn(err)
		errorMail(err, ms)
		return
	}
	defer sender.Close()
	message := gomail.NewMessage()
	currentSize := 0
	for i, m := range ms {
		select {
		case <-ctx.Done():
			return
		default:
			break
		}
		if mw.batchSize > 0 && currentSize >= mw.batchSize {
			time.Sleep(mw.batchWait)
			currentSize = 0
		}
		currentSize++
		message.Reset()
		err = m.Generate(message)
		if err != nil {
			log.Warn(err)
			m.Error(err)
			continue
		}
		err = gomail.SendCustomFrom(sender, smtpFrom, message)
		if err != nil {
			if te, ok := err.(*textproto.Error); ok {
				switch {
				// If it's a temporary error, we should backoff and try again later.
				// We'll reset the connection so future messages don't incur a
				// different error (see https://github.com/gophish/gophish/issues/787).
				case te.Code >= 400 && te.Code <= 499:
					log.WithFields(logrus.Fields{
						"code":  te.Code,
						"email": message.GetHeader("To")[0],
					}).Warn(err)
					m.Backoff(err)
					sender.Reset()
					continue
				// Otherwise, if it's a permanent error, we shouldn't backoff this message,
				// since the RFC specifies that running the same commands won't work next time.
				// We should reset our sender and error this message out.
				case te.Code >= 500 && te.Code <= 599:
					log.WithFields(logrus.Fields{
						"code":  te.Code,
						"email": message.GetHeader("To")[0],
					}).Warn(err)
					m.Error(err)
					sender.Reset()
					continue
				// If something else happened, let's just error out and reset the
				// sender
				default:
					log.WithFields(logrus.Fields{
						"code":  "unknown",
						"email": message.GetHeader("To")[0],
					}).Warn(err)
					m.Error(err)
					sender.Reset()
					continue
				}
			} else {
				// This likely indicates that something happened to the underlying
				// connection. We'll try to reconnect and, if that fails, we'll
				// error out the remaining emails.
				log.WithFields(logrus.Fields{
					"email": message.GetHeader("To")[0],
				}).Warn(err)
				origErr := err
				sender, err = dialHost(ctx, dialer)
				if err != nil {
					errorMail(err, ms[i:])
					break
				}
				m.Backoff(origErr)
				continue
			}
		}
		log.WithFields(logrus.Fields{
			"smtp_from":     smtpFrom,
			"envelope_from": message.GetHeader("From")[0],
			"email":         message.GetHeader("To")[0],
		}).Info("Email sent")
		m.Success()
	}
}
