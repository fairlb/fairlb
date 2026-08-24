import * as echarts from "echarts/core";
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
 */
echarts.use([
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

export { echarts };
