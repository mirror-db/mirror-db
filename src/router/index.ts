import { createRouter, createWebHistory } from "vue-router";
import HomeView from "../views/HomeView.vue";
import EndpointList from "../views/EndpointList.vue";

const router = createRouter({
  history: createWebHistory(import.meta.env.BASE_URL),
  routes: [
    {
      path: "/",
      name: "home",
      component: HomeView,
    },
    {
      path: "/endpoints",
      name: "endpoints",
      component: EndpointList,
    },
  ],
});

export default router;
