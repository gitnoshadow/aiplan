import { createApp } from 'vue'
import { Button, NavBar, Empty } from 'vant'
import 'vant/es/style/base.css'
import 'vant/es/button/style'
import 'vant/es/nav-bar/style'
import 'vant/es/empty/style'
import 'vant/es/loading/style'
import './style.css'
import App from './App.vue'

createApp(App).use(Button).use(NavBar).use(Empty).mount('#app')
