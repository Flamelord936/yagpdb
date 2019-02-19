package soundboard

import (
	"context"
	"github.com/jonas747/dca"
	"github.com/jonas747/yagpdb/commands"
	"github.com/jonas747/yagpdb/common"
	"github.com/jonas747/yagpdb/common/backgroundworkers"
	"github.com/jonas747/yagpdb/soundboard/models"
	"github.com/sirupsen/logrus"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	transcoderStop    = make(chan *sync.WaitGroup)
	transcoderOptions *dca.EncodeOptions
)

func init() {
	// Copy the standard options
	cp := *dca.StdEncodeOptions
	transcoderOptions = &cp
	transcoderOptions.Bitrate = 100
}

var _ commands.CommandProvider = (*Plugin)(nil)
var _ backgroundworkers.BackgroundWorkerPlugin = (*Plugin)(nil)

func (p *Plugin) RunBackgroundWorker() {
	transcoderLoop()
}
func (p *Plugin) StopBackgroundWorker(wg *sync.WaitGroup) {
	logrus.Info("Stopping soundboard transcoder...")

	transcoderStop <- wg
}

func transcoderLoop() {
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case wg := <-transcoderStop:
			wg.Done()
			return
		case <-ticker.C:
			items := getQueue()
			for _, v := range items {
				started := time.Now()
				logrus.Println("handling queue item")
				err := handleQueueItem(v)
				logrus.Println("done handling queue item")
				if err != nil {
					logrus.WithError(err).WithField("soundid", v).Error("Failed processing transcode queue item")
				}
				logrus.WithField("sounf", v).Info("Took ", time.Since(started).String(), " to transcode sound ")
			}
		}
	}
}

func getQueue() []string {
	files, err := ioutil.ReadDir("soundboard/queue")
	if err != nil {
		logrus.WithError(err).Error("Failed checking queue directory")
		return []string{}
	}

	out := make([]string, len(files))

	for k, v := range files {
		out[k] = v.Name()
	}

	return out
}

func handleQueueItem(item string) error {
	skipTranscode := false

	idStr := item
	if strings.HasSuffix(item, ".dca") {
		skipTranscode = true
		idStr = strings.SplitN(item, ".", 2)[0]
	}

	parsedId, err := strconv.Atoi(idStr)
	if err != nil {
		return err
	}

	// lock it for max 10 minutes, after that something must've gone wrong
	locked, err := common.TryLockRedisKey(KeySoundLock(parsedId), 10*60)
	if err != nil {
		return err
	}
	if !locked {
		logrus.WithField("sound", parsedId).Warn("Sound is busy, handling it later")
		return nil
	}
	defer common.UnlockRedisKey(KeySoundLock(parsedId))

	sound, err := models.FindSoundboardSoundG(context.Background(), parsedId)
	if err != nil {
		return err
	}

	logrus.WithField("sound", sound.ID).Info("Handling queued sound ", sound.Name)

	if !skipTranscode {
		err = transcodeSound(sound)
		if err != nil {
			logrus.WithError(err).WithField("sound", sound.ID).Error("Failed transcoding sound")
			common.GORM.Model(&sound).Update("Status", TranscodingStatusFailedOther)
			os.Remove(SoundFilePath(sound.ID, TranscodingStatusReady))
		} else {
			common.GORM.Model(&sound).Update("Status", TranscodingStatusReady)
		}

		err = os.Remove(SoundFilePath(sound.ID, TranscodingStatusQueued))
	} else {
		os.Rename(SoundFilePath(sound.ID, TranscodingStatusQueued)+".dca", SoundFilePath(sound.ID, TranscodingStatusReady))
	}
	return err
}

func transcodeSound(sound *models.SoundboardSound) error {
	destFile, err := os.Create(SoundFilePath(sound.ID, TranscodingStatusReady))
	if err != nil {
		return err
	}
	defer destFile.Close()

	session, err := dca.EncodeFile(SoundFilePath(sound.ID, TranscodingStatus(sound.Status)), transcoderOptions)
	if err != nil {
		return err
	}

	_, err = io.Copy(destFile, session)
	if err != nil {
		session.Truncate()
		return err
	}
	err = session.Error()

	return err
}
