// The runtime of the curve.
//
// The library of the curve is large, so it is loaded on demand: a page that
// shows no curve does not carry it. The parts of the library that the
// curve needs are named one by one, so the whole of it never travels to
// the browser.
import { LineChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { init, use, type ECharts } from 'echarts/core'
import { CanvasRenderer } from 'echarts/renderers'

use([LineChart, GridComponent, LegendComponent, TooltipComponent, CanvasRenderer])

// createChart draws a curve in the given element.
export function createChart(element: HTMLElement): ECharts {
  return init(element)
}
