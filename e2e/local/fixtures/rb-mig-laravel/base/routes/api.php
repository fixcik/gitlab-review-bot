<?php

use App\Http\Controllers\OrderController;
use Illuminate\Support\Facades\Route;

Route::get('/orders/mine', [OrderController::class, 'mine']);
Route::post('/orders', [OrderController::class, 'store']);
