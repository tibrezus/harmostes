import { useEffect, useState } from "react";
import { PipelineList } from "./pages/PipelineList";
import { PipelineEditor } from "./pages/PipelineEditor";

// Simple path-based router. The Go server serves this SPA at:
//   /pipelines          → list page
//   /pipelines/new      → new pipeline editor
//   /pipelines/{name}   → existing pipeline editor
function parseRoute(): { page: "list" | "editor"; name?: string } {
  const path = window.location.pathname.replace(/\/+$/, "");
  if (path === "/pipelines" || path === "/pipelines/") {
    return { page: "list" };
  }
  if (path === "/pipelines/new") {
    return { page: "editor" };
  }
  const match = path.match(/^\/pipelines\/(.+)$/);
  if (match) {
    return { page: "editor", name: decodeURIComponent(match[1]) };
  }
  return { page: "list" };
}

export default function App() {
  const [route, setRoute] = useState(parseRoute());

  useEffect(() => {
    const onPop = () => setRoute(parseRoute());
    window.addEventListener("popstate", onPop);
    return () => window.removeEventListener("popstate", onPop);
  }, []);

  if (route.page === "editor") {
    return <PipelineEditor name={route.name} />;
  }
  return <PipelineList />;
}
