package forecast

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/bradfitz/iter"
	dataframe "github.com/rocketlaunchr/dataframe-go"
	"github.com/rocketlaunchr/dataframe-go/utils/utime"
)

// HwModel is a Model Interface that holds necessary
// computed values for a forecasting result
type HwModel struct {
	data                 []float64
	trainData            []float64
	testData             []float64
	initialSmooth        float64
	initialTrend         float64
	initialSeasonalComps []float64
	smoothingLevel       float64
	trendLevel           float64
	seasonalComps        []float64
	period               int
	alpha                float64
	beta                 float64
	gamma                float64
	inputIsDf            bool
	tsInterval           string
	tsIntReverse         bool
	tsName               string
	lastTsVal            time.Time
}

func NewHwModel() *HwModel {
	model := &HwModel{
		data:                 []float64{},
		trainData:            []float64{},
		testData:             []float64{},
		initialSmooth:        0.0,
		initialTrend:         0.0,
		initialSeasonalComps: []float64{},
		smoothingLevel:       0.0,
		trendLevel:           0.0,
		seasonalComps:        []float64{},
		period:               0,
		alpha:                0.0,
		beta:                 0.0,
		gamma:                0.0,
		inputIsDf:            false,
	}

	return model
}

// Configure sets the various parameters for Ets Algorithm.
// config must be a HoltWintersConfig struct.
func (hm *HwModel) Configure(config interface{}) {
	if cfg, ok := config.(*HoltWintersConfig); ok {

		if (cfg.Alpha < 0.0) || (cfg.Alpha > 1.0) {
			panic("alpha must be between [0,1]")
		}

		if (cfg.Beta < 0.0) || (cfg.Beta > 1.0) {
			panic("beta must be between [0,1]")
		}

		if (cfg.Gamma < 0.0) || (cfg.Gamma > 1.0) {
			panic("gamma must be between [0,1]")
		}

		hm.alpha = cfg.Alpha
		hm.beta = cfg.Beta
		hm.gamma = cfg.Gamma
		hm.period = cfg.Period

	} else {
		panic(fmt.Errorf("struct config parameter [%T] is not compartible with ets model Type: [*forecast.HoltWintersConfig]", cfg))
	}
}

// Load loads historical data. sdf can be a SeriesFloat64 or DataFrame.
func (hm *HwModel) Load(sdf interface{}, r *dataframe.Range) {
	ctx := context.Background()

	switch d := sdf.(type) {
	case *dataframe.SeriesFloat64:

		hm.data = d.Values

	case *dataframe.DataFrame:

		err := hm.loadDataFromDF(ctx, d)
		if err != nil {
			panic(err)
		}

	default:
		panic("unknown data format passed in. make sure you pass in a SeriesFloat64 or a forecast standard two(2) column dataframe")
	}

	tr := &dataframe.Range{}
	if r != nil {
		tr = r
	}

	count := len(hm.data)
	if count == 0 {
		panic(ErrNoRows)
	}

	start, end, err := tr.Limits(count)
	if err != nil {
		panic(err)
	}

	// Validation
	if end-start < 1 {
		panic("no values in selected series range")
	}

	trainData := hm.data[start : end+1]
	hm.trainData = trainData

	testData := hm.data[end+1:]
	if len(testData) < 3 {
		panic("there should be a minimum of 3 data left as testing data")
	}
	hm.testData = testData

	err = hm.trainModel(ctx, start, end)
	if err != nil {
		panic(err)
	}
}

func (hm *HwModel) loadDataFromDF(ctx context.Context, d *dataframe.DataFrame) error {

	var (
		data      []float64
		isDf      bool
		tsInt     string
		tReverse  bool
		err       error
		tsName    string
		lastTsVal time.Time
	)

	isDf = true
	// validate that
	// - DataFrame has exactly two columns
	// - first column is SeriesTime
	// - second column is SeriesFloat64
	if len(d.Series) != 2 {

		return errors.New("dataframe passed in must have exactly two series/columns")
	}

	if d.Series[0].Type() == "time" {
		// get the current time interval/freq from the seriesTime
		if ts, ok := d.Series[0].(*dataframe.SeriesTime); ok {
			tsName = ts.Name(dataframe.DontLock)

			rowLen := ts.NRows(dataframe.DontLock)
			// store the last value in timeSeries column
			lastTsVal = ts.Value(rowLen-1, dataframe.DontLock).(time.Time)

			// guessing with only half the original time series row length
			// for efficiency
			half := rowLen / 2
			utimeOpts := utime.GuessTimeFreqOptions{
				R:        &dataframe.Range{End: &half},
				DontLock: true,
			}

			tsInt, tReverse, err = utime.GuessTimeFreq(ctx, ts, utimeOpts)
			if err != nil {
				return err
			}
		} else {
			return errors.New("column 0 not convertible to SeriesTime")
		}
	} else {
		return errors.New("first column/series must be a SeriesTime")
	}

	if d.Series[1].Type() == "float64" {
		val := d.Series[1].Copy()
		if v, ok := val.(*dataframe.SeriesFloat64); ok {
			data = v.Values
		} else {
			return errors.New("column 1 not convertible to SeriesFloat64")
		}
	} else {
		return errors.New("second column/series must be a SeriesFloat64")
	}

	hm.data = data
	hm.inputIsDf = isDf
	hm.tsInterval = tsInt
	hm.tsIntReverse = tReverse
	hm.tsName = tsName
	hm.lastTsVal = lastTsVal

	return nil
}

func (hm *HwModel) trainModel(ctx context.Context, start, end int) error {
	var (
		α, β, γ        float64
		period         int
		trnd, prevTrnd float64 // trend
		st, prevSt     float64 // smooth
	)

	α = hm.alpha
	β = hm.beta
	γ = hm.gamma
	period = hm.period

	y := hm.data[start : end+1]

	seasonals := initialSeasonalComponents(y, period)

	hm.initialSeasonalComps = initialSeasonalComponents(y, period)

	trnd = initialTrend(y, period)
	hm.initialTrend = trnd

	for i := start; i < end+1; i++ {
		// Breaking out on context failure
		if err := ctx.Err(); err != nil {
			return err
		}

		xt := y[i]

		if i == start { // Set initial smooth
			st = xt

			hm.initialSmooth = xt

		} else {
			// multiplicative method
			// prevSt, st = st, α * (xt / seasonals[i % period]) + (1 - α) * (st + trnd)
			// prevTrnd, trnd = trnd, β * (st - prevSt) + (1 - β) * trnd
			// seasonals[i % period] = γ * (xt / (prevSt + prevTrnd)) + (1 - γ) * seasonals[i % period]

			// additive method
			prevSt, st = st, α*(xt-seasonals[i%period])+(1-α)*(st+trnd)
			prevTrnd, trnd = trnd, β*(st-prevSt)+(1-β)*trnd
			seasonals[i%period] = γ*(xt-prevSt-prevTrnd) + (1-γ)*seasonals[i%period]
			// _ = prevTrnd
			// fmt.Println(st + trnd + seasonals[i % period])
		}

	}

	hm.smoothingLevel = st
	hm.trendLevel = trnd
	hm.period = period
	hm.seasonalComps = seasonals

	return nil
}

// Validate can be used by providing a validation set of data.
// It will then forecast the values from the end of the loaded data and then compare
// them with the validation set.
func (hm *HwModel) Validate(ctx context.Context, sdf interface{}, r *dataframe.Range, errorType ErrorType) (float64, error) {

	var (
		actualDataset   *dataframe.SeriesFloat64
		forecastDataset *dataframe.SeriesFloat64
		errVal          float64
	)

	tr := &dataframe.Range{}
	if r != nil {
		tr = r
	}

	switch d := sdf.(type) {
	case *dataframe.SeriesFloat64:

		val := d.Copy(*tr)
		if v, ok := val.(*dataframe.SeriesFloat64); ok {
			actualDataset = v
		} else {
			return math.NaN(), errors.New("series data is not SeriesFloat64")
		}

	case *dataframe.DataFrame:

		if val, ok := d.Series[1].(*dataframe.SeriesFloat64); ok {
			actualDataset = val.Copy(*tr).(*dataframe.SeriesFloat64)
		} else {
			return math.NaN(), errors.New("series data is not SeriesFloat64")
		}

	default:
		return math.NaN(), errors.New("unknown data format passed in. make sure you pass in a SeriesFloat64 or a forecast standard two(2) column dataframe")
	}

	m := len(actualDataset.Values)
	forecast, err := hm.Predict(ctx, m)
	if err != nil {
		return math.NaN(), err
	}

	switch f := forecast.(type) {
	case *dataframe.SeriesFloat64:
		forecastDataset = f
	case *dataframe.DataFrame:
		forecastDataset = f.Series[1].(*dataframe.SeriesFloat64)
	}

	// Calculate error measurement between forecast and actual dataSet
	errOpts := &ErrorOptions{}

	if errorType == MAE {
		errVal, _, err = MeanAbsoluteError(ctx, actualDataset, forecastDataset, errOpts)
		if err != nil {
			return math.NaN(), err
		}
	} else if errorType == SSE {
		errVal, _, err = SumOfSquaredErrors(ctx, actualDataset, forecastDataset, errOpts)
		if err != nil {
			return math.NaN(), err
		}
	} else if errorType == RMSE {
		errVal, _, err = RootMeanSquaredError(ctx, actualDataset, forecastDataset, errOpts)
		if err != nil {
			return math.NaN(), err
		}
	} else if errorType == MAPE {
		errVal, _, err = MeanAbsolutePercentageError(ctx, actualDataset, forecastDataset, errOpts)
		if err != nil {
			return math.NaN(), err
		}
	} else {
		return math.NaN(), errors.New("Unknown error type")
	}

	return errVal, nil
}

// Predict forecasts the next n values for a Series or DataFrame.
// If a Series was provided to Load function, then a Series is retured.
// Alternatively a DataFrame is returned.
func (hm *HwModel) Predict(ctx context.Context, n int) (interface{}, error) {

	// Validation
	if n <= 0 {
		return nil, errors.New("value of h must be greater than 0")
	}

	forecast := make([]float64, n)

	st := hm.smoothingLevel
	seasonals := hm.seasonalComps
	trnd := hm.trendLevel
	period := hm.period

	m := 1
	pos := 0
	for range iter.N(n) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// multiplicative Method
		// fcast = append(fcast, (st + float64(m)*trnd) * seasonals[(m-1) % period])

		// additive method
		forecast[pos] = (st + float64(m)*trnd) + seasonals[(m-1)%period]

		m++
		pos++
	}

	fdf := dataframe.NewSeriesFloat64("Prediction", nil)
	fdf.Values = forecast

	if hm.inputIsDf {
		size := n + 1

		// generate SeriesTime to start and continue from where it stopped in data input
		opts := utime.NewSeriesTimeOptions{
			Size: &size,
		}
		ts, err := utime.NewSeriesTime(ctx, hm.tsName, hm.tsInterval, hm.lastTsVal, hm.tsIntReverse, opts)
		if err != nil {
			panic(fmt.Errorf("error encountered while generating time interval prediction: %v", err))
		}

		// trying to exclude the first starting time
		nTs := ts.Copy(dataframe.Range{Start: &[]int{1}[0]})

		// combine fdf and generated time series into a dataframe and return
		return dataframe.NewDataFrame(nTs, fdf), nil
	}

	return fdf, nil
}
