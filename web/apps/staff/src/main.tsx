import { mountApp } from "@fairlb/app-composition";
import { RouterProvider } from "@tanstack/react-router";
import { createAdminRouter } from "./router";
import "./styles.css";

const router = createAdminRouter();

// The router's `Register` type is intentionally not declared: the route tree is
// generated from the registry, so the literal paths cannot be enumerated at the
// type level. Link and useParams fall back to plain strings.

mountApp({
  root: document.getElementById("root")!,
  app: <RouterProvider router={router} />,
  appNameKey: "appCommunityAdmin",
});
