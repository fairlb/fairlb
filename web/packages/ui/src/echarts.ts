import * as echartsCore from "echarts/core";
import { BarChart, LineChart } from "echarts/charts";
import {
  AriaComponent,
  BrushComponent,
  GridComponent,
  MarkLineComponent,
  ToolboxComponent,
  TooltipComponent,
} from "echarts/components";
import { CanvasRenderer } from "echarts/renderers";

/**
 * Register only the charting modules actually used.
 *
 * The chart components take the charting instance as a prop rather than
 * importing it themselves, precisely so the caller decides what gets bundled.
 * This application draws two shapes — a time series line/bar and a magnitude bar
 * — so these registrations are enough; importing the library's default entry
 * would pull in maps, graphs, 3D and the rest.
 *
 * A new chart type has to be registered here first. Without that, the runtime
 * only warns in the console and the chart renders empty.
 *
 * The registration is the export's initialiser rather than a free-standing
 * `echarts.use(...)` statement, and the difference is load-bearing: this package
 * declares `"sideEffects": false`, so a production build is entitled to drop any
 * statement that does not contribute to an export. A bare `use()` call is
 * exactly such a statement — Rollup removed it, the painter registry stayed
 * empty, and the first chart threw `el[a] is not a constructor` in every
 * production bundle while dev and vitest (no tree-shaking) stayed green.
 */
function registered(): typeof echartsCore {
  echartsCore.use([
    LineChart,
    BarChart,
    GridComponent,
    TooltipComponent,
    MarkLineComponent,
    AriaComponent,
    BrushComponent,
    ToolboxComponent,
    CanvasRenderer,
  ]);
  return echartsCore;
}

export const echarts = registered();
