package config

import (
	"log"
	"math"
	"runtime"
	"strconv"

	"github.com/spf13/viper"
)

// Read - Reading .env file content, during application start up
func Read(file string) error {
	viper.SetConfigFile(file)

	return viper.ReadInConfig()
}

// Get - Get config value by key
func Get(key string) string {
	return viper.GetString(key)
}

// GetFloat - Parse confiig value as floating point number & return
func GetFloat(key string) float64 {
	return viper.GetFloat64(key)
}

// GetMemPoolPollingPeriod - Read mempool polling period & attempt to
// parse it to string, where it's expected that this period will be
// provided in form of time duration with millisecond level precision
//
// Example: If you want to poll mempool content every 2 seconds, you must be
// writing 2000 in `.env` file
//
// If you don't provide any value for this expected field, by default it'll
// start using 1000ms i.e. after completion of this iteration, it'll sleep for
// 1000ms & again get to work
func GetMemPoolPollingPeriod() uint64 {

	period := Get("MemPoolPollingPeriod")

	_period, err := strconv.ParseUint(period, 10, 64)
	if err != nil {
		log.Printf("[❗️] Failed to parse mempool polling period : `%s`, using 1000 ms\n", err.Error())
		return 1000
	}

	return _period

}

// GetPendingTxEntryPublishTopic - Read provided topic name from `.env` file
// where newly added pending pool tx(s) to be published
func GetPendingTxEntryPublishTopic() string {

	if v := Get("PendingTxEntryTopic"); len(v) != 0 {
		return v
	}

	log.Printf("[❗️] Failed to get topic for publishing new pending tx, using `pending_pool_entry`\n")
	return "pending_pool_entry"

}

// GetPendingTxExitPublishTopic - Read provided topic name from `.env` file
// where tx(s) removed from pending pool to be published
func GetPendingTxExitPublishTopic() string {

	if v := Get("PendingTxExitTopic"); len(v) != 0 {
		return v
	}

	log.Printf("[❗️] Failed to get topic for publishing tx removed from pending pool, using `pending_pool_exit`\n")
	return "pending_pool_exit"

}

// GetQueuedTxEntryPublishTopic - Read provided topic name from `.env` file
// where newly added queued pool tx(s) to be published
func GetQueuedTxEntryPublishTopic() string {

	if v := Get("QueuedTxEntryTopic"); len(v) != 0 {
		return v
	}

	log.Printf("[❗️] Failed to get topic for publishing new queued tx, using `queued_pool_entry`\n")
	return "queued_pool_entry"

}

// GetQueuedTxExitPublishTopic - Read provided topic name from `.env` file
// where tx(s) removed from queued pool to be published
func GetQueuedTxExitPublishTopic() string {

	if v := Get("QueuedTxExitTopic"); len(v) != 0 {
		return v
	}

	log.Printf("[❗️] Failed to get topic for publishing tx removed from queued pool, using `queued_pool_exit`\n")
	return "queued_pool_exit"

}

// GetRedisDBIndex - Read desired redis database index, which
// user asked `harmony` to use
//
// If nothing is provided, it'll use `1`, by default
func GetRedisDBIndex() uint8 {

	db := Get("RedisDB")

	_db, err := strconv.ParseUint(db, 10, 8)
	if err != nil {
		log.Printf("[❗️] Failed to parse redis database index : `%s`, using 1\n", err.Error())
		return 1
	}

	return uint8(_db)

}

// GetConcurrencyFactor - Size of worker pool, is dictated by rule below
//
// @note You can set floating point value for `ConcurrencyFactor` ( > 0 )
func GetConcurrencyFactor() int {

	f := int(math.Ceil(GetFloat("ConcurrencyFactor") * float64(runtime.NumCPU())))
	if f <= 0 {

		log.Printf("[❗️] Bad concurrency factor, using unit sized pool\n")
		return 1

	}

	return f

}